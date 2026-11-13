// Package gateway is Homeport's edge gateway. It terminates members'
// WireGuard tunnels in userspace, counts every inner IP byte per peer, and
// relays each TCP or UDP flow over QUIC to the agent of the line the peer
// uses (or to a FlowDialer, in tests).
package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"

	"github.com/jermainee/homeport/gateway/internal/relay"
	"github.com/jermainee/homeport/gateway/internal/wgstack"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
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

	// QUICListen is the UDP address sharing agents dial, such as ":4433".
	// Certificate is the gateway's relay certificate; agents pin its key.
	QUICListen  string
	Certificate tls.Certificate

	// DNS is the virtual resolver in members' WireGuard configs. Flows to
	// port 53 there become DNS flows the agent resolves. 0 means 10.77.0.1.
	DNS netip.Addr

	// Dialer replaces the relay to agents, for tests. Without QUICListen it
	// is required.
	Dialer FlowDialer

	// MaxPendingFlowsPerPeer caps the flows one tunnel address may have
	// waiting on the Dialer; more are refused with a reset. 0 means 64.
	MaxPendingFlowsPerPeer int
}

// Peer is a member device allowed to connect.
type Peer struct {
	PublicKey  Key
	IPv4, IPv6 netip.Addr // tunnel addresses; at least one is required
	MemberID   string     // the member who owns the device
	LineID     string     // the line its traffic exits through
}

// Line is a member's shared line, served by the agent with AgentPin.
type Line struct {
	ID            string
	OwnerMemberID string
	AgentPin      relayprotocol.Pin
}

// errRefused refuses a flow; the client sees a reset.
var errRefused = errors.New("gateway: flow refused")

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
	relay  *relay.Relay // nil when Config.Dialer replaces it
	port   uint16
	mu     sync.Mutex
	peers  map[Key]Peer
	byAddr map[netip.Addr]Key
	lines  map[string]Line
	closed bool
}

// New starts a gateway listening for WireGuard on cfg.ListenPort.
func New(cfg Config) (*Server, error) {
	if cfg.Dialer == nil && cfg.QUICListen == "" {
		return nil, errors.New("gateway: Config.QUICListen or Config.Dialer is required")
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
	if !cfg.DNS.IsValid() {
		cfg.DNS = netip.MustParseAddr("10.77.0.1")
	}
	s := &Server{cfg: cfg, peers: make(map[Key]Peer), byAddr: make(map[netip.Addr]Key), lines: make(map[string]Line)}
	dialer := cfg.Dialer
	if dialer == nil {
		r, err := relay.Listen(cfg.QUICListen, cfg.Certificate, netip.AddrPortFrom(cfg.DNS, 53))
		if err != nil {
			return nil, fmt.Errorf("gateway: %w", err)
		}
		s.relay = r
		dialer = router{s}
	}
	s.meter = wgstack.NewMeter()
	st, err := wgstack.New(cfg.MTU, s.meter, dialer, cfg.MaxPendingFlowsPerPeer)
	if err != nil {
		s.closeRelay()
		return nil, fmt.Errorf("gateway: %w", err)
	}
	dev := device.NewDevice(st, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	uapi := fmt.Sprintf("private_key=%s\nlisten_port=%d\n", hex.EncodeToString(cfg.PrivateKey[:]), cfg.ListenPort)
	fail := func(err error) (*Server, error) {
		dev.Close()
		s.closeRelay()
		return nil, err
	}
	if err := dev.IpcSet(uapi); err != nil {
		return fail(fmt.Errorf("gateway: configure WireGuard: %w", err))
	}
	if err := dev.Up(); err != nil {
		return fail(fmt.Errorf("gateway: start WireGuard: %w", err))
	}
	port, err := listenPort(dev)
	if err != nil {
		return fail(fmt.Errorf("gateway: %w", err))
	}
	s.dev, s.port = dev, port
	return s, nil
}

// QUICAddr is the UDP address agents dial, or nil without a relay.
func (s *Server) QUICAddr() net.Addr {
	if s.relay == nil {
		return nil
	}
	return s.relay.Addr()
}

// RelayPin is the pin of the gateway's relay certificate, which agents accept.
func (s *Server) RelayPin() relayprotocol.Pin {
	if s.relay == nil {
		return relayprotocol.Pin{}
	}
	return s.relay.Pin()
}

// AddLine lets the agent with l.AgentPin connect and serve line l.ID, or
// updates the line. A changed pin disconnects the old agent.
func (s *Server) AddLine(l Line) error {
	if s.relay == nil {
		return errors.New("gateway: no relay configured")
	}
	if l.ID == "" || l.AgentPin == (relayprotocol.Pin{}) {
		return errors.New("gateway: line needs an ID and an agent pin")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines[l.ID] = l
	s.relay.SetLine(relay.Line{ID: l.ID, Pin: l.AgentPin})
	return nil
}

// RemoveLine disconnects the line's agent and refuses flows to it from now on.
func (s *Server) RemoveLine(id string) {
	if s.relay == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lines, id)
	s.relay.RemoveLine(id)
}

// LineOnline reports whether the line's agent is connected.
func (s *Server) LineOnline(id string) bool {
	return s.relay != nil && s.relay.Online(id)
}

// router sends each flow to the agent of the line its peer uses, after
// checking that the line exists and the peer does not own it.
type router struct{ s *Server }

func (r router) DialTCP(ctx context.Context, f Flow) (net.Conn, error) {
	lineID, err := r.s.lineFor(f.Peer)
	if err != nil {
		return nil, err
	}
	return r.s.relay.DialTCP(ctx, lineID, f.Dst)
}

func (r router) DialUDP(ctx context.Context, f Flow) (net.Conn, error) {
	lineID, err := r.s.lineFor(f.Peer)
	if err != nil {
		return nil, err
	}
	return r.s.relay.DialUDP(ctx, lineID, f.Dst)
}

// lineFor returns the line a peer's flows exit through. A member never
// exits through their own line.
func (s *Server) lineFor(addr netip.Addr) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[s.byAddr[addr]]
	if !ok || p.LineID == "" {
		return "", errRefused
	}
	l, ok := s.lines[p.LineID]
	if !ok || l.OwnerMemberID == p.MemberID {
		return "", errRefused
	}
	return l.ID, nil
}

func (s *Server) closeRelay() {
	if s.relay != nil {
		s.relay.Close()
	}
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
	if old, ok := s.peers[p.PublicKey]; ok {
		delete(s.byAddr, old.IPv4)
		delete(s.byAddr, old.IPv6)
	}
	for _, a := range []netip.Addr{p.IPv4, p.IPv6} {
		if a.IsValid() {
			s.byAddr[a] = p.PublicKey
		}
	}
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
	p, ok := s.peers[publicKey]
	if !ok {
		return Usage{}, nil
	}
	if err := s.dev.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", hex.EncodeToString(publicKey[:]))); err != nil {
		return Usage{}, fmt.Errorf("gateway: remove peer: %w", err)
	}
	up, down := s.meter.Remove(wgstack.PeerID(publicKey))
	delete(s.byAddr, p.IPv4)
	delete(s.byAddr, p.IPv6)
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
	s.closeRelay()
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
