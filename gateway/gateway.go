// Package gateway is Homeport's edge gateway. It terminates members'
// WireGuard tunnels in userspace, counts every inner IP byte per peer, and
// passes each TCP or UDP flow to a FlowDialer.
package gateway

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"

	"github.com/jermainee/homeport/gateway/internal/wgstack"
)

// Flow is one connection from a peer: its tunnel address and its destination.
type Flow = wgstack.Flow

// FlowDialer opens the far side of a flow. An error refuses the flow.
type FlowDialer = wgstack.Dialer

// Config configures a Server.
type Config struct {
	PrivateKey Key
	ListenPort uint16 // UDP port for WireGuard; 0 picks a free one
	MTU        int    // inner MTU; 0 means 1280
	Dialer     FlowDialer

	// MaxPendingFlowsPerPeer caps the flows one tunnel address may have
	// waiting on the Dialer; more are refused with a reset. 0 means 64.
	MaxPendingFlowsPerPeer int
}

// Peer is a member device allowed to connect.
type Peer struct {
	PublicKey  Key
	IPv4, IPv6 netip.Addr // tunnel addresses; at least one is required
}

// Usage is the inner IP bytes counted for one peer.
type Usage struct {
	Up   uint64 // from the peer
	Down uint64 // to the peer
}

// Server is a running gateway.
type Server struct {
	cfg    Config
	dev    *device.Device
	meter  *wgstack.Meter
	port   uint16
	mu     sync.Mutex
	peers  map[Key]Peer
	closed bool
}

// New starts a gateway listening for WireGuard on cfg.ListenPort.
func New(cfg Config) (*Server, error) {
	if cfg.Dialer == nil {
		return nil, errors.New("gateway: Config.Dialer is required")
	}
	if cfg.PrivateKey == (Key{}) {
		return nil, errors.New("gateway: Config.PrivateKey is required")
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1280
	}
	if cfg.MaxPendingFlowsPerPeer == 0 {
		cfg.MaxPendingFlowsPerPeer = 64
	}
	meter := wgstack.NewMeter()
	st, err := wgstack.New(cfg.MTU, meter, cfg.Dialer, cfg.MaxPendingFlowsPerPeer)
	if err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
	dev := device.NewDevice(st, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	uapi := fmt.Sprintf("private_key=%s\nlisten_port=%d\n", hex.EncodeToString(cfg.PrivateKey[:]), cfg.ListenPort)
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("gateway: configure WireGuard: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("gateway: start WireGuard: %w", err)
	}
	port, err := listenPort(dev)
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("gateway: %w", err)
	}
	return &Server{cfg: cfg, dev: dev, meter: meter, port: port, peers: make(map[Key]Peer)}, nil
}

// ListenPort is the UDP port WireGuard listens on.
func (s *Server) ListenPort() uint16 { return s.port }

// PublicKey is the gateway's WireGuard public key.
func (s *Server) PublicKey() Key { return s.cfg.PrivateKey.PublicKey() }

// AddPeer lets a member device connect, or updates its tunnel addresses.
// Those addresses are the only sources it may send from, and its traffic is
// metered from now on. Re-adding a peer keeps the usage counted so far.
func (s *Server) AddPeer(p Peer) error {
	if !p.IPv4.IsValid() && !p.IPv6.IsValid() {
		return errors.New("gateway: peer needs a tunnel address")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "public_key=%s\nreplace_allowed_ips=true\n", hex.EncodeToString(p.PublicKey[:]))
	if p.IPv4.IsValid() {
		fmt.Fprintf(&b, "allowed_ip=%s\n", netip.PrefixFrom(p.IPv4, 32))
	}
	if p.IPv6.IsValid() {
		fmt.Fprintf(&b, "allowed_ip=%s\n", netip.PrefixFrom(p.IPv6, 128))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, other := range s.peers {
		if key != p.PublicKey && sharesAddress(p, other) {
			return fmt.Errorf("gateway: tunnel address of peer %s is already in use", p.PublicKey)
		}
	}
	if err := s.dev.IpcSet(b.String()); err != nil {
		return fmt.Errorf("gateway: add peer: %w", err)
	}
	s.meter.Set(wgstack.PeerID(p.PublicKey), p.IPv4, p.IPv6)
	s.peers[p.PublicKey] = p
	return nil
}

func sharesAddress(a, b Peer) bool {
	return (a.IPv4.IsValid() && a.IPv4 == b.IPv4) || (a.IPv6.IsValid() && a.IPv6 == b.IPv6)
}

// RemovePeer disconnects a member device and returns its final usage, so
// bytes counted since the last report can still be billed. Its packets are
// refused from now on.
func (s *Server) RemovePeer(publicKey Key) (Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.peers[publicKey]; !ok {
		return Usage{}, nil
	}
	if err := s.dev.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", hex.EncodeToString(publicKey[:]))); err != nil {
		return Usage{}, fmt.Errorf("gateway: remove peer: %w", err)
	}
	up, down := s.meter.Remove(wgstack.PeerID(publicKey))
	delete(s.peers, publicKey)
	return Usage{Up: up, Down: down}, nil
}

// PeerUsage returns the bytes counted for the peer with this tunnel address.
func (s *Server) PeerUsage(addr netip.Addr) Usage {
	up, down := s.meter.Usage(addr)
	return Usage{Up: up, Down: down}
}

// Close stops the gateway.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.dev.Close()
	return nil
}

func listenPort(dev *device.Device) (uint16, error) {
	state, err := dev.IpcGet()
	if err != nil {
		return 0, fmt.Errorf("read WireGuard state: %w", err)
	}
	sc := bufio.NewScanner(strings.NewReader(state))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "listen_port="); ok {
			p, err := strconv.ParseUint(v, 10, 16)
			if err != nil {
				return 0, fmt.Errorf("parse listen port: %w", err)
			}
			return uint16(p), nil
		}
	}
	return 0, errors.New("WireGuard reported no listen port")
}
