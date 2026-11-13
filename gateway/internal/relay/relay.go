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

// Relay accepts agent connections and opens flows on them.
type Relay struct {
	ln     *quic.Listener
	pin    relayprotocol.Pin
	dns    netip.AddrPort
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	lines  map[string]Line              // by line ID
	byPin  map[relayprotocol.Pin]string // agent pin → line ID
	agents map[string]*quic.Conn        // line ID → the agent's connection
}

// Listen starts accepting agents on addr. Flows to dns become DNS flows.
func Listen(addr string, cert tls.Certificate, dns netip.AddrPort) (*Relay, error) {
	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("relay: certificate has no usable private key")
	}
	pin, err := relayprotocol.PinOfPublicKey(signer.Public())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Relay{
		pin:    pin,
		dns:    dns,
		ctx:    ctx,
		cancel: cancel,
		lines:  make(map[string]Line),
		byPin:  make(map[relayprotocol.Pin]string),
		agents: make(map[string]*quic.Conn),
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
// agent that used the old one.
func (r *Relay) SetLine(l Line) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.lines[l.ID]; ok && old.Pin != l.Pin {
		delete(r.byPin, old.Pin)
		r.dropAgentLocked(l.ID, relayprotocol.CodeUnauthorized)
	}
	r.lines[l.ID] = l
	r.byPin[l.Pin] = l.ID
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
	conn := r.agents[lineID]
	r.mu.Unlock()
	if conn == nil {
		return nil, nil, ErrLineOffline
	}
	s, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("relay: open flow: %w", err)
	}
	if err := relayprotocol.WriteFlowOpen(s, relayprotocol.FlowOpen{Kind: kind, Dst: dst}); err != nil {
		s.CancelRead(relayprotocol.CodeFlowCancelled.StreamErrorCode())
		s.CancelWrite(relayprotocol.CodeFlowCancelled.StreamErrorCode())
		return nil, nil, fmt.Errorf("relay: write flow header: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		s.SetReadDeadline(deadline)
	}
	res, err := relayprotocol.ReadFlowResult(s)
	s.SetReadDeadline(time.Time{})
	if err != nil || res.Status != relayprotocol.StatusOK {
		s.CancelRead(relayprotocol.CodeFlowCancelled.StreamErrorCode())
		s.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("relay: read flow result: %w", err)
		}
		return nil, nil, fmt.Errorf("relay: agent answered %s", res.Status)
	}
	return s, conn, nil
}

func (r *Relay) knownPin(p relayprotocol.Pin) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.byPin[p]
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
		conn.CloseWithError(relayprotocol.CodeUnauthorized.ApplicationErrorCode(), "")
		return
	}
	r.mu.Lock()
	lineID, ok := r.byPin[pin]
	r.mu.Unlock()
	if !ok {
		conn.CloseWithError(relayprotocol.CodeUnauthorized.ApplicationErrorCode(), "")
		return
	}

	ctx, cancel := context.WithTimeout(r.ctx, helloTimeout)
	ctl, err := conn.AcceptStream(ctx)
	cancel()
	if err != nil {
		conn.CloseWithError(relayprotocol.CodeProtocolError.ApplicationErrorCode(), "")
		return
	}
	ctl.SetReadDeadline(time.Now().Add(helloTimeout))
	msg, err := relayprotocol.ReadControl(ctl)
	ctl.SetReadDeadline(time.Time{})
	if _, isHello := msg.(relayprotocol.Hello); err != nil || !isHello {
		conn.CloseWithError(relayprotocol.CodeProtocolError.ApplicationErrorCode(), "")
		return
	}
	// Register before acknowledging, so an agent that has its hello_ack can
	// already be sent flows.
	if !r.register(lineID, pin, conn) {
		conn.CloseWithError(relayprotocol.CodeUnauthorized.ApplicationErrorCode(), "")
		return
	}
	defer r.unregister(lineID, conn)
	ack := relayprotocol.HelloAck{LineID: lineID, ObservedIP: remoteIP(conn)}
	if err := relayprotocol.WriteControl(ctl, ack); err != nil {
		conn.CloseWithError(relayprotocol.CodeInternalError.ApplicationErrorCode(), "")
		return
	}
	for {
		if _, err := relayprotocol.ReadControl(ctl); err != nil && !errors.Is(err, relayprotocol.ErrUnknownMessage) {
			return
		}
	}
}

// register makes conn the line's agent, replacing an earlier connection. It
// fails if the line was removed or its pin changed during the hello.
func (r *Relay) register(lineID string, pin relayprotocol.Pin, conn *quic.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.lines[lineID]; !ok || l.Pin != pin {
		return false
	}
	if old := r.agents[lineID]; old != nil {
		old.CloseWithError(relayprotocol.CodeGoingAway.ApplicationErrorCode(), "replaced")
	}
	r.agents[lineID] = conn
	return true
}

func (r *Relay) unregister(lineID string, conn *quic.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agents[lineID] == conn {
		delete(r.agents, lineID)
	}
}

func (r *Relay) dropAgentLocked(lineID string, code relayprotocol.Code) {
	if c := r.agents[lineID]; c != nil {
		c.CloseWithError(code.ApplicationErrorCode(), "")
		delete(r.agents, lineID)
	}
}

func remoteIP(conn *quic.Conn) netip.Addr {
	if a, ok := conn.RemoteAddr().(*net.UDPAddr); ok {
		return a.AddrPort().Addr().Unmap()
	}
	return netip.Addr{}
}
