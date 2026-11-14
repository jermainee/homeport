// Package relay is the gateway's side of the QUIC relay to sharing agents.
// Agents dial in with a pinned client certificate; each pin belongs to one
// line. The gateway opens one stream per flow on the agent's connection, as
// relay-protocol specifies.
package relay

import (
	"context"
	"crypto"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

const helloTimeout = 10 * time.Second

// ErrLineOffline means no agent is connected for the line.
var ErrLineOffline = errors.New("relay: line is offline")

// Line is a member's shared line and the pin of the agent that serves it.
type Line struct {
	ID  string
	Pin relayprotocol.Pin
}

// Event tells that a line's agent registered (Online) or stopped serving
// the line.
type Event struct {
	LineID       string
	Online       bool
	ObservedIP   netip.Addr // the agent's public IP as the gateway sees it
	AgentVersion string     // from the agent's hello
}

// Relay accepts agent connections and opens flows on them.
type Relay struct {
	ln      *quic.Listener
	pin     relayprotocol.Pin
	dns     netip.AddrPort
	onEvent func(Event)
	unknown func()
	ctx     context.Context
	cancel  context.CancelFunc

	mu     sync.Mutex
	lines  map[string]Line              // by line ID
	byPin  map[relayprotocol.Pin]string // agent pin → line ID
	agents map[string]*agent            // line ID → its registered agent
}

// agent is the registered connection of a line's agent.
type agent struct {
	conn    *quic.Conn
	ip      netip.Addr
	version string
}

// Listen starts accepting agents on addr. Flows to dns become DNS flows.
// onEvent, if not nil, is called while the relay holds its lock whenever a
// line goes online or offline, so events arrive in order; it must return
// quickly and must not call the relay. onUnknown, if not nil, is called
// during the handshake of an agent whose pin belongs to no line, before the
// relay refuses it; it must return quickly.
func Listen(addr string, cert tls.Certificate, dns netip.AddrPort, onEvent func(Event), onUnknown func()) (*Relay, error) {
	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("relay: certificate has no usable private key")
	}
	pin, err := relayprotocol.PinOfPublicKey(signer.Public())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if onEvent == nil {
		onEvent = func(Event) {}
	}
	if onUnknown == nil {
		onUnknown = func() {}
	}
	r := &Relay{
		pin:     pin,
		dns:     dns,
		onEvent: onEvent,
		unknown: onUnknown,
		ctx:     ctx,
		cancel:  cancel,
		lines:   make(map[string]Line),
		byPin:   make(map[relayprotocol.Pin]string),
		agents:  make(map[string]*agent),
	}
	ln, err := quic.ListenAddr(addr, relayprotocol.GatewayTLSConfig(cert, r.knownPin), relayprotocol.GatewayQUICConfig())
	if err != nil {
		cancel()
		return nil, fmt.Errorf("relay: listen: %w", err)
	}
	r.ln = ln
	go r.acceptLoop()
	return r, nil
}

// Addr is the UDP address agents dial.
func (r *Relay) Addr() net.Addr { return r.ln.Addr() }

// Pin is the SPKI pin agents must accept for this gateway.
func (r *Relay) Pin() relayprotocol.Pin { return r.pin }

// SetLine adds a line or changes its agent pin. A changed pin disconnects the
// agent that used the old one. A pin can serve only one line.
func (r *Relay) SetLine(l Line) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if other, ok := r.byPin[l.Pin]; ok && other != l.ID {
		return fmt.Errorf("relay: the agent pin of line %s already serves another line", l.ID)
	}
	if old, ok := r.lines[l.ID]; ok && old.Pin != l.Pin {
		delete(r.byPin, old.Pin)
		r.dropAgentLocked(l.ID, relayprotocol.CodeUnauthorized)
	}
	r.lines[l.ID] = l
	r.byPin[l.Pin] = l.ID
	return nil
}

// RemoveLine forgets a line and disconnects its agent.
func (r *Relay) RemoveLine(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.lines[id]; ok {
		delete(r.byPin, l.Pin)
		delete(r.lines, id)
	}
	r.dropAgentLocked(id, relayprotocol.CodeUnauthorized)
}

// Online reports whether an agent is connected for the line.
func (r *Relay) Online(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agents[id] != nil
}

// DialTCP opens a TCP flow (or DNS over TCP to the virtual resolver) through
// the line's agent. It returns once the agent has answered.
func (r *Relay) DialTCP(ctx context.Context, lineID string, dst netip.AddrPort) (net.Conn, error) {
	kind := relayprotocol.KindTCP
	if dst == r.dns {
		kind = relayprotocol.KindDNS
	}
	s, conn, err := r.openFlow(ctx, lineID, kind, dst)
	if err != nil {
		return nil, err
	}
	return &streamConn{stream: s, conn: conn}, nil
}

// DialUDP opens a UDP flow (or a DNS flow to the virtual resolver) through
// the line's agent. Each Read and Write is one datagram.
func (r *Relay) DialUDP(ctx context.Context, lineID string, dst netip.AddrPort) (net.Conn, error) {
	kind := relayprotocol.KindUDP
	if dst == r.dns {
		kind = relayprotocol.KindDNS
	}
	s, conn, err := r.openFlow(ctx, lineID, kind, dst)
	if err != nil {
		return nil, err
	}
	return &frameConn{streamConn{stream: s, conn: conn}}, nil
}

// Close stops accepting agents and disconnects them all.
func (r *Relay) Close() error {
	r.cancel()
	r.mu.Lock()
	for id := range r.agents {
		r.dropAgentLocked(id, relayprotocol.CodeGoingAway)
	}
	r.mu.Unlock()
	return r.ln.Close()
}

func (r *Relay) openFlow(ctx context.Context, lineID string, kind relayprotocol.Kind, dst netip.AddrPort) (*quic.Stream, *quic.Conn, error) {
	r.mu.Lock()
	a := r.agents[lineID]
	r.mu.Unlock()
	if a == nil {
		return nil, nil, ErrLineOffline
	}
	conn := a.conn
	s, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("relay: open flow: %w", err)
	}
	if err := relayprotocol.WriteFlowOpen(s, relayprotocol.FlowOpen{Kind: kind, Dst: dst}); err != nil {
		s.CancelRead(relayprotocol.CodeFlowCancelled.StreamErrorCode())
		s.CancelWrite(relayprotocol.CodeFlowCancelled.StreamErrorCode())
		return nil, nil, fmt.Errorf("relay: write flow header: %w", err)
	}
	// Wait for the agent's answer until the context ends, by deadline or by
	// cancellation (for example when the gateway closes).
	stop := context.AfterFunc(ctx, func() { _ = s.SetReadDeadline(time.Now()) })
	res, err := relayprotocol.ReadFlowResult(s)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	_ = s.SetReadDeadline(time.Time{})
	if err != nil || res.Status != relayprotocol.StatusOK {
		s.CancelRead(relayprotocol.CodeFlowCancelled.StreamErrorCode())
		_ = s.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("relay: read flow result: %w", err)
		}
		return nil, nil, fmt.Errorf("relay: agent answered %s", res.Status)
	}
	return s, conn, nil
}

// knownPin is the handshake's check of an agent's pin. An unknown pin is
// reported, so the gateway can learn a line created since its last config.
func (r *Relay) knownPin(p relayprotocol.Pin) bool {
	r.mu.Lock()
	_, ok := r.byPin[p]
	r.mu.Unlock()
	if !ok {
		r.unknown()
	}
	return ok
}

func (r *Relay) acceptLoop() {
	for {
		conn, err := r.ln.Accept(r.ctx)
		if err != nil {
			return
		}
		go r.serve(conn)
	}
}

// serve runs one agent connection: hello, registration, then control
// messages until the connection ends.
func (r *Relay) serve(conn *quic.Conn) {
	pin, err := relayprotocol.PeerPin(conn.ConnectionState().TLS)
	if err != nil {
		_ = conn.CloseWithError(relayprotocol.CodeUnauthorized.ApplicationErrorCode(), "")
		return
	}
	r.mu.Lock()
	lineID, ok := r.byPin[pin]
	r.mu.Unlock()
	if !ok {
		_ = conn.CloseWithError(relayprotocol.CodeUnauthorized.ApplicationErrorCode(), "")
		return
	}

	ctx, cancel := context.WithTimeout(r.ctx, helloTimeout)
	ctl, err := conn.AcceptStream(ctx)
	cancel()
	if err != nil {
		_ = conn.CloseWithError(relayprotocol.CodeProtocolError.ApplicationErrorCode(), "")
		return
	}
	_ = ctl.SetReadDeadline(time.Now().Add(helloTimeout))
	msg, err := relayprotocol.ReadControl(ctl)
	_ = ctl.SetReadDeadline(time.Time{})
	hello, isHello := msg.(relayprotocol.Hello)
	if err != nil || !isHello {
		_ = conn.CloseWithError(relayprotocol.CodeProtocolError.ApplicationErrorCode(), "")
		return
	}
	// Register before acknowledging, so an agent that has its hello_ack can
	// already be sent flows.
	if !r.register(lineID, pin, &agent{conn: conn, ip: remoteIP(conn), version: hello.AgentVersion}) {
		_ = conn.CloseWithError(relayprotocol.CodeUnauthorized.ApplicationErrorCode(), "")
		return
	}
	defer r.unregister(lineID, conn)
	ack := relayprotocol.HelloAck{LineID: lineID, ObservedIP: remoteIP(conn)}
	if err := relayprotocol.WriteControl(ctl, ack); err != nil {
		_ = conn.CloseWithError(relayprotocol.CodeInternalError.ApplicationErrorCode(), "")
		return
	}
	for {
		if _, err := relayprotocol.ReadControl(ctl); err != nil && !errors.Is(err, relayprotocol.ErrUnknownMessage) {
			// The control stream ended or broke. Close the connection so the
			// agent reconnects instead of idling on a line nobody routes to.
			_ = conn.CloseWithError(relayprotocol.CodeProtocolError.ApplicationErrorCode(), "")
			return
		}
	}
}

// register makes a the line's agent, replacing an earlier connection, and
// reports the line online. It fails if the line was removed or its pin
// changed during the hello.
func (r *Relay) register(lineID string, pin relayprotocol.Pin, a *agent) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return false // the relay is closing
	}
	if l, ok := r.lines[lineID]; !ok || l.Pin != pin {
		return false
	}
	if old := r.agents[lineID]; old != nil {
		// The new connection's online event follows; the line never went offline.
		_ = old.conn.CloseWithError(relayprotocol.CodeGoingAway.ApplicationErrorCode(), "replaced")
	}
	r.agents[lineID] = a
	r.onEvent(Event{LineID: lineID, Online: true, ObservedIP: a.ip, AgentVersion: a.version})
	return true
}

func (r *Relay) unregister(lineID string, conn *quic.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.agents[lineID]; a != nil && a.conn == conn {
		delete(r.agents, lineID)
		r.onEvent(Event{LineID: lineID, ObservedIP: a.ip, AgentVersion: a.version})
	}
}

func (r *Relay) dropAgentLocked(lineID string, code relayprotocol.Code) {
	if a := r.agents[lineID]; a != nil {
		_ = a.conn.CloseWithError(code.ApplicationErrorCode(), "")
		delete(r.agents, lineID)
		r.onEvent(Event{LineID: lineID, ObservedIP: a.ip, AgentVersion: a.version})
	}
}

func remoteIP(conn *quic.Conn) netip.Addr {
	if a, ok := conn.RemoteAddr().(*net.UDPAddr); ok {
		return a.AddrPort().Addr().Unmap()
	}
	return netip.Addr{}
}
