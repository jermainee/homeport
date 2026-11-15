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
	"sync/atomic"
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
	// DNS flows end soon after their answer. Each one holds a stream on the
	// agent, and every query from a new source port is a new flow.
	dnsIdleTimeout = 5 * time.Second
	// A TLS-only flow must send its ClientHello this soon.
	helloTimeout = 10 * time.Second
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

// Policy decides whether a flow may open. Admit runs before the flow is
// dialed and may block until ctx ends.
type Policy interface {
	Admit(ctx context.Context, f Flow) Admission
}

// Admission is a Policy's answer for one flow.
type Admission struct {
	Allow bool
	// ServerName, when set, admits only TLS: a TCP flow opens if the server
	// name in its ClientHello is one ServerName accepts, and a UDP flow is
	// refused. The client's handshake completes before the dial, so such a
	// flow can no longer be refused with a plain reset during the handshake.
	ServerName func(name string) bool
	// TLSOnly admits a TCP flow only if it opens with a TLS ClientHello,
	// with any server name or none. Unlike ServerName, the destination is
	// dialed first, so an unreachable one is refused at the SYN as usual;
	// nothing goes upstream until the ClientHello has arrived, and a server
	// that speaks first is refused at once. UDP flows are not affected.
	TLSOnly bool
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

	policy Policy // nil allows every flow

	maxPending int // dials one tunnel address may have open at once
	pendingMu  sync.Mutex
	pending    map[netip.Addr]int

	flowsMu sync.Mutex
	flows   map[netip.Addr]map[*flowEntry]struct{} // open flows by tunnel address
	epochs  map[netip.Addr]uint64                  // ResetFlows calls per tunnel address
}

// flowEntry is an open flow that ResetFlows can end.
type flowEntry struct{ reset func() }

// New builds the stack. The meter decides which peers may send, the policy
// (if not nil) which flows they may open, and each tunnel address may have
// at most maxPending flows waiting on the policy and the dialer.
func New(mtu int, meter *Meter, dialer Dialer, policy Policy, maxPending int) (*Stack, error) {
	if mtu < 1280 || mtu > 65535 {
		return nil, fmt.Errorf("MTU %d outside 1280..65535", mtu)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Stack{
		ctx:        ctx,
		cancel:     cancel,
		maxPending: maxPending,
		pending:    make(map[netip.Addr]int),
		flows:      make(map[netip.Addr]map[*flowEntry]struct{}),
		epochs:     make(map[netip.Addr]uint64),
		policy:     policy,
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
		if !ok || !s.meter.countUp(src, uint64(len(packet))) {
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
// against the peer it goes to. ICMP is never sent: in promiscuous mode,
// gVisor's IPv6 layer answers echo requests for any destination, which
// would let a peer see another member's tunnel address, or any address,
// answer as if it were reachable. The gateway has no other ICMP for peers:
// every TCP and UDP flow ends at its own stack.
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
		if !ok || isICMP(packet) || !s.meter.countDown(dst, uint64(len(packet))) {
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
	adm, epoch := s.admit(flow)
	if !adm.Allow {
		s.endPending(flow.Peer)
		r.Complete(true)
		return
	}
	if adm.ServerName != nil {
		s.handleTLSOnly(r, flow, epoch, adm.ServerName)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	defer cancel()
	upstream, err := s.dialer.DialTCP(ctx, flow)
	s.endPending(flow.Peer)
	if err != nil {
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		r.Complete(true)
		_ = upstream.Close()
		return
	}
	r.Complete(false)
	client := gonet.NewTCPConn(&wq, ep)
	var hello []byte
	if adm.TLSOnly {
		if hello, err = awaitClientHello(client, upstream, time.Now().Add(helloTimeout)); err != nil {
			ep.Abort()
			_ = client.Close()
			if rs, ok := upstream.(interface{ Reset() }); ok {
				rs.Reset()
			}
			_ = upstream.Close()
			return
		}
	}
	s.relayTCP(flow.Peer, epoch, client, ep.Abort, upstream, hello)
}

// errServerFirst is a server that spoke before the client's ClientHello.
var errServerFirst = errors.New("wgstack: the server spoke first, so this is not TLS")

// awaitClientHello reads the client's ClientHello for a TLS-only flow and
// returns its bytes, to be sent upstream first. Meanwhile it watches the
// upstream: a server that sends anything first is not speaking TLS, which
// is always client-first, so the flow is refused at once instead of
// waiting for a ClientHello that never comes. Nothing is sent upstream.
func awaitClientHello(client, upstream net.Conn, deadline time.Time) ([]byte, error) {
	var spoke atomic.Bool
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		var b [1]byte
		if n, _ := upstream.Read(b[:]); n > 0 {
			spoke.Store(true)
			_ = client.SetReadDeadline(time.Now()) // end the peek
		}
	}()
	_, hello, err := peekServerName(client, deadline)
	// Stop the watcher and wait for it, so only the relay reads upstream
	// from here on. A read woken by the deadline consumes nothing.
	_ = upstream.SetReadDeadline(time.Now())
	<-watched
	_ = upstream.SetReadDeadline(time.Time{})
	if spoke.Load() {
		return nil, errServerFirst
	}
	return hello, err
}

// handleTLSOnly completes the client's handshake, reads its ClientHello,
// and dials only if accept takes the server name it names ("" for none).
// The ClientHello is then passed on unchanged.
func (s *Stack) handleTLSOnly(r *tcp.ForwarderRequest, flow Flow, epoch uint64, accept func(string) bool) {
	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		s.endPending(flow.Peer)
		r.Complete(true)
		return
	}
	r.Complete(false)
	client := gonet.NewTCPConn(&wq, ep)
	refuse := func() {
		s.endPending(flow.Peer)
		ep.Abort()
		_ = client.Close()
	}
	name, hello, err := peekServerName(client, time.Now().Add(helloTimeout))
	if err != nil || !accept(name) {
		refuse()
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	defer cancel()
	upstream, err := s.dialer.DialTCP(ctx, flow)
	if err != nil {
		refuse()
		return
	}
	s.endPending(flow.Peer)
	s.relayTCP(flow.Peer, epoch, client, ep.Abort, upstream, hello)
}

// admit asks the policy about a flow and returns its answer with the epoch
// it holds for: a reset of the peer's flows after that epoch ends the flow.
// When a reset lands while the policy decides, the answer may predate it,
// so the policy is asked again. A peer whose flows keep being reset is
// refused.
func (s *Stack) admit(f Flow) (Admission, uint64) {
	epoch := s.epoch(f.Peer)
	if s.policy == nil {
		return Admission{Allow: true}, epoch
	}
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	defer cancel()
	for range 3 {
		adm := s.policy.Admit(ctx, f)
		now := s.epoch(f.Peer)
		if now == epoch {
			return adm, epoch
		}
		epoch = now
	}
	return Admission{}, epoch
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
		adm, epoch := s.admit(flow)
		if !adm.Allow || adm.ServerName != nil {
			_ = client.Close()
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
		defer cancel()
		upstream, err := s.dialer.DialUDP(ctx, flow)
		if err != nil {
			_ = client.Close()
			return
		}
		cancel()
		idle := udpIdleTimeout
		if flow.Dst.Port() == 53 {
			idle = dnsIdleTimeout
		}
		s.relayUDP(flow.Peer, epoch, client, upstream, idle)
	}()
}

// epoch is the number of ResetFlows calls for addr so far. A flow admitted
// before a reset is ended as soon as it is tracked.
func (s *Stack) epoch(addr netip.Addr) uint64 {
	s.flowsMu.Lock()
	defer s.flowsMu.Unlock()
	return s.epochs[addr]
}

// track registers an open flow of peer. It reports false, and registers
// nothing, when the peer's flows were reset since epoch.
func (s *Stack) track(peer netip.Addr, epoch uint64, reset func()) (untrack func(), ok bool) {
	s.flowsMu.Lock()
	defer s.flowsMu.Unlock()
	if s.epochs[peer] != epoch {
		return nil, false
	}
	e := &flowEntry{reset: reset}
	if s.flows[peer] == nil {
		s.flows[peer] = make(map[*flowEntry]struct{})
	}
	s.flows[peer][e] = struct{}{}
	return func() {
		s.flowsMu.Lock()
		defer s.flowsMu.Unlock()
		delete(s.flows[peer], e)
		if len(s.flows[peer]) == 0 {
			delete(s.flows, peer)
		}
	}, true
}

// ResetFlows ends every open flow of these tunnel addresses, including
// flows still waiting on the policy or the dialer. A TCP client gets a
// reset.
func (s *Stack) ResetFlows(addrs ...netip.Addr) {
	var resets []func()
	s.flowsMu.Lock()
	for _, a := range addrs {
		if !a.IsValid() {
			continue
		}
		s.epochs[a]++
		for e := range s.flows[a] {
			resets = append(resets, e.reset)
		}
		delete(s.flows, a)
	}
	s.flowsMu.Unlock()
	for _, reset := range resets {
		reset()
	}
}

// relayTCP copies both ways between the client and the upstream, after
// sending first (the ClientHello a TLS-only flow already read) upstream. A
// clean end of one direction is passed on as a half-close. An error on
// either side is a reset: the client gets a TCP reset (abortClient) and the
// upstream is reset too, so a cut-off response never looks complete. Close
// on the stack, or ResetFlows for the peer, ends both.
func (s *Stack) relayTCP(peer netip.Addr, epoch uint64, client net.Conn, abortClient func(), upstream net.Conn, first []byte) {
	closeBoth := func() {
		_ = client.Close()
		_ = upstream.Close()
	}
	resetBoth := func() {
		abortClient()
		if r, ok := upstream.(interface{ Reset() }); ok {
			r.Reset()
		}
		closeBoth()
	}
	stop := context.AfterFunc(s.ctx, closeBoth)
	defer stop()
	untrack, ok := s.track(peer, epoch, resetBoth)
	if !ok {
		resetBoth()
		return
	}
	defer untrack()
	if len(first) > 0 {
		if _, err := upstream.Write(first); err != nil {
			resetBoth()
			return
		}
	}
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if err != nil {
			resetBoth()
		} else if cw, ok := dst.(interface{ CloseWrite() error }); !ok || cw.CloseWrite() != nil {
			closeBoth()
		}
		done <- struct{}{}
	}
	go pipe(client, upstream)
	go pipe(upstream, client)
	<-done
	<-done
	closeBoth()
}

// relayUDP copies datagrams both ways until the flow is idle for idle, the
// stack closes or ResetFlows ends the peer's flows.
func (s *Stack) relayUDP(peer netip.Addr, epoch uint64, client, upstream net.Conn, idle time.Duration) {
	closeBoth := func() {
		_ = client.Close()
		_ = upstream.Close()
	}
	defer closeBoth()
	stop := context.AfterFunc(s.ctx, closeBoth)
	defer stop()
	untrack, ok := s.track(peer, epoch, closeBoth)
	if !ok {
		return
	}
	defer untrack()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 65535)
		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))
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

// isICMP reports whether one of the stack's own packets carries ICMP or
// ICMPv6. The stack puts no extension headers before ICMPv6.
func isICMP(packet []byte) bool {
	switch packet[0] >> 4 {
	case 4:
		return header.IPv4(packet).TransportProtocol() == header.ICMPv4ProtocolNumber
	case 6:
		return header.IPv6(packet).TransportProtocol() == header.ICMPv6ProtocolNumber
	}
	return false
}

var _ tun.Device = (*Stack)(nil)
