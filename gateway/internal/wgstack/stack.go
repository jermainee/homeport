// Package wgstack is the gateway's userspace network stack. WireGuard hands
// it decrypted packets from every peer; it accepts each TCP and UDP flow,
// whatever the destination, and passes the flow to a Dialer.
package wgstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	nicID          = 1
	maxTCPInFlight = 1024
	dialTimeout    = 10 * time.Second
	udpIdleTimeout = 60 * time.Second
)

// Flow is one connection from a peer: who opened it and where it goes.
type Flow struct {
	Peer netip.Addr     // the peer's tunnel address
	Dst  netip.AddrPort // the destination the peer asked for
}

// Dialer opens the far side of a flow. An error refuses the flow: a TCP
// client gets a reset, a UDP packet is dropped.
type Dialer interface {
	DialTCP(ctx context.Context, f Flow) (net.Conn, error)
	DialUDP(ctx context.Context, f Flow) (net.Conn, error)
}

// Stack is a tun.Device backed by a gVisor stack in promiscuous mode.
type Stack struct {
	ep        *channel.Endpoint
	stack     *stack.Stack
	notify    *channel.NotificationHandle
	events    chan tun.Event
	incoming  chan *buffer.View
	closed    chan struct{}
	closeOnce sync.Once
	ctx       context.Context // cancelled on Close; ends dials and relays
	cancel    context.CancelFunc
	mtu       int
	meter     *Meter
	dialer    Dialer

	maxPending int // dials one tunnel address may have open at once
	pendingMu  sync.Mutex
	pending    map[netip.Addr]int
}

// New builds the stack. The meter decides which peers may send, and each
// tunnel address may have at most maxPending flows waiting on the dialer.
func New(mtu int, meter *Meter, dialer Dialer, maxPending int) (*Stack, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Stack{
		ctx:        ctx,
		cancel:     cancel,
		maxPending: maxPending,
		pending:    make(map[netip.Addr]int),
		ep:         channel.New(1024, uint32(mtu), ""),
		stack: stack.New(stack.Options{
			NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
			TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		}),
		events:   make(chan tun.Event, 1),
		incoming: make(chan *buffer.View),
		closed:   make(chan struct{}),
		mtu:      mtu,
		meter:    meter,
		dialer:   dialer,
	}
	sack := tcpip.TCPSACKEnabled(true)
	if err := s.stack.SetTransportProtocolOption(tcp.ProtocolNumber, &sack); err != nil {
		return nil, fmt.Errorf("enable TCP SACK: %v", err)
	}
	if err := s.stack.CreateNIC(nicID, s.ep); err != nil {
		return nil, fmt.Errorf("create NIC: %v", err)
	}
	// Accept packets for any destination and answer from that destination.
	if err := s.stack.SetPromiscuousMode(nicID, true); err != nil {
		return nil, fmt.Errorf("promiscuous mode: %v", err)
	}
	if err := s.stack.SetSpoofing(nicID, true); err != nil {
		return nil, fmt.Errorf("spoofing: %v", err)
	}
	s.stack.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})
	tcpFwd := tcp.NewForwarder(s.stack, 0, maxTCPInFlight, s.handleTCP)
	s.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(s.stack, s.handleUDP)
	s.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	s.notify = s.ep.AddNotify(s)
	s.events <- tun.EventUp
	return s, nil
}

// Write receives decrypted packets from WireGuard peers. Packets whose
// source the meter does not know are dropped uncounted.
func (s *Stack) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		packet := b[offset:]
		src, proto, ok := sourceOf(packet)
		if !ok || !s.meter.countUp(src, len(packet)) {
			continue
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet)})
		s.ep.InjectInbound(proto, pkt)
		pkt.DecRef()
	}
	return len(bufs), nil
}

// WriteNotify is called by the channel endpoint when the stack sends a packet.
func (s *Stack) WriteNotify() {
	pkt := s.ep.Read()
	if pkt == nil {
		return
	}
	view := pkt.ToView()
	pkt.DecRef()
	select {
	case s.incoming <- view:
	case <-s.closed:
		view.Release()
	}
}

// Read hands the stack's outgoing packets to WireGuard, counting each one
// against the peer it goes to.
func (s *Stack) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	for {
		var view *buffer.View
		select {
		case view = <-s.incoming:
		case <-s.closed:
			return 0, os.ErrClosed
		}
		n, err := view.Read(bufs[0][offset:])
		view.Release()
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		packet := bufs[0][offset : offset+n]
		dst, ok := destinationOf(packet)
		if !ok || !s.meter.countDown(dst, n) {
			continue
		}
		sizes[0] = n
		return 1, nil
	}
}

func (s *Stack) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	flow := Flow{
		Peer: addrFrom(id.RemoteAddress),
		Dst:  netip.AddrPortFrom(addrFrom(id.LocalAddress), id.LocalPort),
	}
	if !s.startPending(flow.Peer) {
		r.Complete(true)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	upstream, err := s.dialer.DialTCP(ctx, flow)
	cancel()
	s.endPending(flow.Peer)
	if err != nil {
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		r.Complete(true)
		upstream.Close()
		return
	}
	r.Complete(false)
	s.relayTCP(gonet.NewTCPConn(&wq, ep), upstream)
}

// startPending reserves one of the peer's pending-dial slots. A peer at its
// limit is refused, so one member cannot use up the forwarder for everyone.
func (s *Stack) startPending(peer netip.Addr) bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pending[peer] >= s.maxPending {
		return false
	}
	s.pending[peer]++
	return true
}

func (s *Stack) endPending(peer netip.Addr) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pending[peer]--; s.pending[peer] <= 0 {
		delete(s.pending, peer)
	}
}

func (s *Stack) handleUDP(r *udp.ForwarderRequest) {
	id := r.ID()
	flow := Flow{
		Peer: addrFrom(id.RemoteAddress),
		Dst:  netip.AddrPortFrom(addrFrom(id.LocalAddress), id.LocalPort),
	}
	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		return
	}
	client := gonet.NewUDPConn(&wq, ep)
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
		upstream, err := s.dialer.DialUDP(ctx, flow)
		cancel()
		if err != nil {
			client.Close()
			return
		}
		s.relayUDP(client, upstream)
	}()
}

// relayTCP copies both ways. A clean end of one direction is passed on as a
// half-close; an error on either side, or Close on the stack, ends both.
func (s *Stack) relayTCP(a, b net.Conn) {
	closeBoth := func() {
		a.Close()
		b.Close()
	}
	stop := context.AfterFunc(s.ctx, closeBoth)
	defer stop()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		cw, halfClose := dst.(interface{ CloseWrite() error })
		if err != nil || !halfClose || cw.CloseWrite() != nil {
			closeBoth()
		}
		done <- struct{}{}
	}
	go pipe(a, b)
	go pipe(b, a)
	<-done
	<-done
	closeBoth()
}

// relayUDP copies datagrams both ways until the flow is idle or the stack closes.
func (s *Stack) relayUDP(client, upstream net.Conn) {
	defer client.Close()
	defer upstream.Close()
	stop := context.AfterFunc(s.ctx, func() {
		client.Close()
		upstream.Close()
	})
	defer stop()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 65535)
		for {
			src.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			n, err := src.Read(buf)
			if err != nil {
				break
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go pipe(upstream, client)
	go pipe(client, upstream)
	<-done
}

// Name, File, Events, MTU, BatchSize and Close complete tun.Device.
func (s *Stack) Name() (string, error)    { return "homeport", nil }
func (s *Stack) File() *os.File           { return nil }
func (s *Stack) Events() <-chan tun.Event { return s.events }
func (s *Stack) MTU() (int, error)        { return s.mtu, nil }
func (s *Stack) BatchSize() int           { return 1 }

// Close stops the stack and every flow through it. It is safe to call more than once.
func (s *Stack) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		close(s.closed)
		s.ep.RemoveNotify(s.notify)
		s.stack.RemoveNIC(nicID)
		s.stack.Close()
		s.ep.Close()
		close(s.events)
	})
	return nil
}

func addrFrom(a tcpip.Address) netip.Addr {
	addr, _ := netip.AddrFromSlice(a.AsSlice())
	return addr
}

func sourceOf(packet []byte) (netip.Addr, tcpip.NetworkProtocolNumber, bool) {
	switch {
	case len(packet) >= header.IPv4MinimumSize && packet[0]>>4 == 4:
		return addrFrom(header.IPv4(packet).SourceAddress()), ipv4.ProtocolNumber, true
	case len(packet) >= header.IPv6MinimumSize && packet[0]>>4 == 6:
		return addrFrom(header.IPv6(packet).SourceAddress()), ipv6.ProtocolNumber, true
	}
	return netip.Addr{}, 0, false
}

func destinationOf(packet []byte) (netip.Addr, bool) {
	switch {
	case len(packet) >= header.IPv4MinimumSize && packet[0]>>4 == 4:
		return addrFrom(header.IPv4(packet).DestinationAddress()), true
	case len(packet) >= header.IPv6MinimumSize && packet[0]>>4 == 6:
		return addrFrom(header.IPv6(packet).DestinationAddress()), true
	}
	return netip.Addr{}, false
}

var _ tun.Device = (*Stack)(nil)
