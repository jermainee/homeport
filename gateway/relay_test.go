package gateway_test

import (
	"context"
	"crypto"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/jermainee/homeport/gateway"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// flowHandler is what a fake agent does with one flow stream after reading
// its FlowOpen.
type flowHandler func(open relayprotocol.FlowOpen, s *quic.Stream)

// fakeAgent is a sharing agent built from relay-protocol, as the real one is.
type fakeAgent struct {
	conn  *quic.Conn
	ctl   *quic.Stream
	ack   relayprotocol.HelloAck
	opens chan relayprotocol.FlowOpen
}

func newAgentCert(t *testing.T) (tls.Certificate, relayprotocol.Pin) {
	t.Helper()
	cert, err := relayprotocol.GenerateSelfSigned("agent")
	if err != nil {
		t.Fatalf("agent certificate: %v", err)
	}
	pin, err := relayprotocol.PinOfPublicKey(cert.PrivateKey.(crypto.Signer).Public())
	if err != nil {
		t.Fatalf("agent pin: %v", err)
	}
	return cert, pin
}

// startAgent connects an agent to the gateway, says hello and serves flows.
func startAgent(t *testing.T, gw *gateway.Server, cert tls.Certificate, handle flowHandler) (*fakeAgent, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, ctl, ack, err := dialAgent(ctx, gw, cert)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = conn.CloseWithError(0, "") })
	a := &fakeAgent{conn: conn, ctl: ctl, ack: ack, opens: make(chan relayprotocol.FlowOpen, 64)}
	go func() {
		for {
			s, err := conn.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				open, err := relayprotocol.ReadFlowOpen(s)
				if err != nil {
					s.CancelRead(relayprotocol.CodeProtocolError.StreamErrorCode())
					return
				}
				select {
				case a.opens <- open:
				default:
				}
				handle(open, s)
			}()
		}
	}()
	return a, nil
}

// dialAgent dials the gateway as an agent with cert and says hello. It
// returns the connection once the gateway acknowledged the hello, or the
// error that ended the attempt, with the connection closed. Saying hello
// also draws the gateway's refusal again if the packet that carried it was
// lost.
func dialAgent(ctx context.Context, gw *gateway.Server, cert tls.Certificate) (*quic.Conn, *quic.Stream, relayprotocol.HelloAck, error) {
	tlsConf := relayprotocol.AgentTLSConfig(cert, relayprotocol.AllowPins(gw.RelayPin()))
	conn, err := quic.DialAddr(ctx, gw.QUICAddr().String(), tlsConf, relayprotocol.AgentQUICConfig())
	if err != nil {
		return nil, nil, relayprotocol.HelloAck{}, err
	}
	fail := func(err error) (*quic.Conn, *quic.Stream, relayprotocol.HelloAck, error) {
		_ = conn.CloseWithError(0, "")
		return nil, nil, relayprotocol.HelloAck{}, err
	}
	ctl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fail(err)
	}
	if err := relayprotocol.WriteControl(ctl, relayprotocol.Hello{AgentVersion: "test", OS: runtime.GOOS}); err != nil {
		return fail(err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = ctl.SetReadDeadline(deadline)
	}
	msg, err := relayprotocol.ReadControl(ctl)
	_ = ctl.SetReadDeadline(time.Time{})
	if err != nil {
		return fail(err)
	}
	ack, ok := msg.(relayprotocol.HelloAck)
	if !ok {
		return fail(errors.New("first control message was not hello_ack"))
	}
	return conn, ctl, ack, nil
}

// relayTo makes the agent connect every TCP flow to origin.
func relayTo(origin string) flowHandler {
	return func(_ relayprotocol.FlowOpen, s *quic.Stream) {
		c, err := net.Dial("tcp", origin)
		if err != nil {
			_ = relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: relayprotocol.StatusRefused})
			_ = s.Close()
			return
		}
		defer func() { _ = c.Close() }()
		if err := relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: relayprotocol.StatusOK}); err != nil {
			return
		}
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(c, s)
			_ = c.(*net.TCPConn).CloseWrite()
			close(done)
		}()
		_, _ = io.Copy(s, c)
		_ = s.Close()
		<-done
	}
}

// relayGateway starts a gateway that relays flows to agents over QUIC.
func relayGateway(t *testing.T) *gateway.Server {
	t.Helper()
	cert, err := relayprotocol.GenerateSelfSigned("gateway")
	if err != nil {
		t.Fatalf("gateway certificate: %v", err)
	}
	return newGatewayWith(t, gateway.Config{QUICListen: "127.0.0.1:0", Certificate: cert})
}

func TestFlowsAreRelayedToTheLinesAgent(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello from home"))
	}))
	t.Cleanup(origin.Close)
	gw := relayGateway(t)
	agentCert, agentPin := newAgentCert(t)
	if err := gw.AddLine(gateway.Line{ID: "line-1", OwnerMemberID: "member-owner", AgentPin: agentPin}); err != nil {
		t.Fatalf("add line: %v", err)
	}
	agent, err := startAgent(t, gw, agentCert, relayTo(origin.Listener.Addr().String()))
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if agent.ack.LineID != "line-1" {
		t.Fatalf("hello_ack named line %q, want line-1", agent.ack.LineID)
	}
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP, LineID: "line-1", MemberID: "member-user"}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peerIP)

	if got := string(client.get(t, "http://11.22.33.44/")); got != "hello from home" {
		t.Fatalf("got %q through the agent", got)
	}
	open := <-agent.opens
	if open.Kind != relayprotocol.KindTCP || open.Dst != netip.MustParseAddrPort("11.22.33.44:80") {
		t.Fatalf("agent got %+v, want TCP to 11.22.33.44:80", open)
	}
}

func TestSetPeerLineSendsTheNextFlowsToThatLinesAgent(t *testing.T) {
	gw := relayGateway(t)
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	for _, line := range []string{"line-1", "line-2"} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("from " + line))
		}))
		t.Cleanup(origin.Close)
		cert, pin := newAgentCert(t)
		if err := gw.AddLine(gateway.Line{ID: line, OwnerMemberID: "owner-of-" + line, AgentPin: pin}); err != nil {
			t.Fatalf("add %s: %v", line, err)
		}
		if _, err := startAgent(t, gw, cert, relayTo(origin.Listener.Addr().String())); err != nil {
			t.Fatalf("agent of %s: %v", line, err)
		}
	}
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP, LineID: "line-1", MemberID: "member-user"}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peerIP)
	before := string(client.get(t, "http://11.22.33.44/"))

	gw.SetPeerLine(key.PublicKey(), "line-2")

	if got := string(client.get(t, "http://11.22.33.44/")); before != "from line-1" || got != "from line-2" {
		t.Fatalf("got %q, then %q after moving the peer; want from line-1, then from line-2", before, got)
	}
}

func TestSetPeerLineNeverSendsAMembersFlowsThroughTheirOwnLine(t *testing.T) {
	gw := relayGateway(t)
	cert, pin := newAgentCert(t)
	if err := gw.AddLine(gateway.Line{ID: "own-line", OwnerMemberID: "member-user", AgentPin: pin}); err != nil {
		t.Fatalf("add line: %v", err)
	}
	agent, err := startAgent(t, gw, cert, answer(relayprotocol.StatusOK))
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP, LineID: "line-1", MemberID: "member-user"}); err != nil {
		t.Fatalf("add peer: %v", err)
	}

	gw.SetPeerLine(key.PublicKey(), "own-line")

	assertResetQuickly(t, newClient(t, gw, key, peerIP))
	select {
	case open := <-agent.opens:
		t.Fatalf("the member's flow reached their own line's agent: %+v", open)
	default:
	}
}

// answer makes the agent answer every flow with status and end it.
func answer(status relayprotocol.Status) flowHandler {
	return func(_ relayprotocol.FlowOpen, s *quic.Stream) {
		_ = relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: status})
		_ = s.Close()
	}
}

// echoFrames makes the agent answer each frame with prefix + the frame, as
// a UDP echo server or a DNS resolver would.
func echoFrames(prefix string) flowHandler {
	return func(_ relayprotocol.FlowOpen, s *quic.Stream) {
		if err := relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: relayprotocol.StatusOK}); err != nil {
			return
		}
		buf := make([]byte, relayprotocol.MaxUDPPayload)
		for {
			payload, err := relayprotocol.ReadUDPFrame(s, buf)
			if err != nil {
				_ = s.Close()
				return
			}
			if err := relayprotocol.WriteUDPFrame(s, append([]byte(prefix), payload...)); err != nil {
				return
			}
		}
	}
}

// relayFixture is a gateway with one line, served by a fake agent, and a
// WireGuard client whose peer uses that line.
type relayFixture struct {
	gw     *gateway.Server
	agent  *fakeAgent
	cert   tls.Certificate
	client *testClient
}

func newRelayFixture(t *testing.T, handle flowHandler, clientMember string) *relayFixture {
	t.Helper()
	gw := relayGateway(t)
	cert, pin := newAgentCert(t)
	if err := gw.AddLine(gateway.Line{ID: "line-1", OwnerMemberID: "member-owner", AgentPin: pin}); err != nil {
		t.Fatalf("add line: %v", err)
	}
	agent, err := startAgent(t, gw, cert, handle)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP, LineID: "line-1", MemberID: clientMember}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	return &relayFixture{gw: gw, agent: agent, cert: cert, client: newClient(t, gw, key, peerIP)}
}

// assertResetQuickly checks that a TCP connection through the gateway is
// refused at once instead of opening or timing out.
func assertResetQuickly(t *testing.T, c *testClient) {
	t.Helper()
	dialRefusedQuickly(t, c, netip.MustParseAddrPort("11.22.33.44:80"))
}

func waitClosed(t *testing.T, conn *quic.Conn) {
	t.Helper()
	select {
	case <-conn.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the agent's connection is still open")
	}
}

func TestUnknownAgentIsRefused(t *testing.T) {
	gw := relayGateway(t)
	cert, _ := newAgentCert(t)
	if _, err := startAgent(t, gw, cert, answer(relayprotocol.StatusOK)); err == nil {
		t.Fatal("an agent whose pin belongs to no line was accepted")
	}
}

func TestFlowIsResetWhenTheLineIsOffline(t *testing.T) {
	gw := relayGateway(t)
	_, pin := newAgentCert(t)
	if err := gw.AddLine(gateway.Line{ID: "line-1", OwnerMemberID: "member-owner", AgentPin: pin}); err != nil {
		t.Fatalf("add line: %v", err)
	}
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP, LineID: "line-1", MemberID: "member-user"}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	assertResetQuickly(t, newClient(t, gw, key, peerIP))
}

func TestAgentRefusalResetsTheClient(t *testing.T) {
	f := newRelayFixture(t, answer(relayprotocol.StatusPolicy), "member-user")
	assertResetQuickly(t, f.client)
}

func TestMemberCannotUseTheirOwnLine(t *testing.T) {
	f := newRelayFixture(t, answer(relayprotocol.StatusOK), "member-owner")
	assertResetQuickly(t, f.client)
	select {
	case open := <-f.agent.opens:
		t.Fatalf("the owner's own flow reached the agent: %+v", open)
	default:
	}
}

func TestNewAgentConnectionReplacesTheOld(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("from the new agent"))
	}))
	t.Cleanup(origin.Close)
	f := newRelayFixture(t, answer(relayprotocol.StatusPolicy), "member-user")

	if _, err := startAgent(t, f.gw, f.cert, relayTo(origin.Listener.Addr().String())); err != nil {
		t.Fatalf("second agent: %v", err)
	}

	waitClosed(t, f.agent.conn)
	if got := string(f.client.get(t, "http://11.22.33.44/")); got != "from the new agent" {
		t.Fatalf("got %q; flows should go to the newer connection", got)
	}
}

func TestRemoveLineDisconnectsItsAgent(t *testing.T) {
	f := newRelayFixture(t, answer(relayprotocol.StatusOK), "member-user")
	if !f.gw.LineOnline("line-1") {
		t.Fatal("line not online after its agent said hello")
	}

	f.gw.RemoveLine("line-1")

	waitClosed(t, f.agent.conn)
	if f.gw.LineOnline("line-1") {
		t.Fatal("removed line still online")
	}
	assertResetQuickly(t, f.client)
}

func TestDNSQueriesBecomeDNSFlows(t *testing.T) {
	f := newRelayFixture(t, echoFrames("answer:"), "member-user")

	reply := f.client.udpRoundTrip(t, netip.MustParseAddrPort("10.77.0.1:53"), []byte("query"))

	if string(reply) != "answer:query" {
		t.Fatalf("got %q from the agent's resolver", reply)
	}
	open := <-f.agent.opens
	if open.Kind != relayprotocol.KindDNS || open.Dst != netip.MustParseAddrPort("10.77.0.1:53") {
		t.Fatalf("agent got %+v, want a DNS flow to 10.77.0.1:53", open)
	}
}

func TestUDPFlowsAreRelayedAsFrames(t *testing.T) {
	f := newRelayFixture(t, echoFrames(""), "member-user")

	reply := f.client.udpRoundTrip(t, netip.MustParseAddrPort("11.22.33.44:7"), []byte("ping"))

	if string(reply) != "ping" {
		t.Fatalf("got %q back", reply)
	}
	open := <-f.agent.opens
	if open.Kind != relayprotocol.KindUDP || open.Dst != netip.MustParseAddrPort("11.22.33.44:7") {
		t.Fatalf("agent got %+v, want UDP to 11.22.33.44:7", open)
	}
}

func TestAgentIsDisconnectedWhenItsControlStreamEnds(t *testing.T) {
	f := newRelayFixture(t, answer(relayprotocol.StatusOK), "member-user")

	_ = f.agent.ctl.Close()

	// A closed connection makes the agent reconnect; an open one would
	// leave it connected to a gateway that no longer routes to it.
	waitClosed(t, f.agent.conn)
}

func TestDNSFlowsCloseSoonAfterTheAnswer(t *testing.T) {
	ended := make(chan struct{}, 8)
	f := newRelayFixture(t, func(open relayprotocol.FlowOpen, s *quic.Stream) {
		echoFrames("answer:")(open, s)
		ended <- struct{}{}
	}, "member-user")

	f.client.udpRoundTrip(t, netip.MustParseAddrPort("10.77.0.1:53"), []byte("query"))

	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the DNS flow still held an agent stream 10 s after its answer")
	}
}

func TestAgentResetReachesTheClientAsAReset(t *testing.T) {
	f := newRelayFixture(t, func(_ relayprotocol.FlowOpen, s *quic.Stream) {
		_ = relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: relayprotocol.StatusOK})
		_, _ = s.Write([]byte("partial"))
		time.Sleep(100 * time.Millisecond)
		s.CancelWrite(relayprotocol.CodeFlowReset.StreamErrorCode())
	}, "member-user")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := f.client.net.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort("11.22.33.44:80"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.ReadAll(conn); err == nil {
		t.Fatal("the client saw an orderly close; a reset must stay a reset, or a truncated response looks complete")
	}
}

func TestAnAgentPinCannotServeTwoLines(t *testing.T) {
	gw := relayGateway(t)
	_, pin := newAgentCert(t)
	if err := gw.AddLine(gateway.Line{ID: "line-1", OwnerMemberID: "member-a", AgentPin: pin}); err != nil {
		t.Fatalf("add line-1: %v", err)
	}
	if err := gw.AddLine(gateway.Line{ID: "line-2", OwnerMemberID: "member-b", AgentPin: pin}); err == nil {
		t.Fatal("a second line was given an agent pin that already serves line-1")
	}
}

func TestPeerOnALineNeedsItsMember(t *testing.T) {
	gw := relayGateway(t)
	err := gw.AddPeer(gateway.Peer{PublicKey: newKey(t).PublicKey(), IPv4: netip.MustParseAddr("10.77.0.2"), LineID: "line-1"})
	if err == nil {
		t.Fatal("a peer with a line but no member was accepted; the self-dealing check needs the member")
	}
}

// lineEvents records the gateway's line events.
type lineEvents struct {
	mu     sync.Mutex
	events []gateway.LineEvent
}

func (l *lineEvents) add(e gateway.LineEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *lineEvents) all() []gateway.LineEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]gateway.LineEvent(nil), l.events...)
}

func TestLineEventsTellWhenAnAgentConnectsAndLeaves(t *testing.T) {
	events := &lineEvents{}
	cert, err := relayprotocol.GenerateSelfSigned("gateway")
	if err != nil {
		t.Fatalf("gateway certificate: %v", err)
	}
	gw := newGatewayWith(t, gateway.Config{QUICListen: "127.0.0.1:0", Certificate: cert, OnLineEvent: events.add})
	agentCert, pin := newAgentCert(t)
	if err := gw.AddLine(gateway.Line{ID: "line-1", OwnerMemberID: "member-owner", AgentPin: pin}); err != nil {
		t.Fatalf("add line: %v", err)
	}

	agent, err := startAgent(t, gw, agentCert, answer(relayprotocol.StatusOK))
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	online := gateway.LineEvent{LineID: "line-1", Online: true, ObservedIP: netip.MustParseAddr("127.0.0.1"), AgentVersion: "test"}
	if got := events.all(); len(got) != 1 || got[0] != online {
		t.Fatalf("events %+v after hello, want %+v", got, online)
	}

	_ = agent.conn.CloseWithError(0, "")
	waitUntil(t, func() bool { return len(events.all()) == 2 })
	offline := online
	offline.Online = false
	if got := events.all()[1]; got != offline {
		t.Errorf("event after the agent left is %+v, want %+v", got, offline)
	}

	if _, err := startAgent(t, gw, agentCert, answer(relayprotocol.StatusOK)); err != nil {
		t.Fatalf("agent reconnect: %v", err)
	}
	gw.RemoveLine("line-1")
	got := events.all()
	if len(got) != 4 || got[2] != online || got[3] != offline {
		t.Errorf("events %+v; want online, then offline when the line is removed", got[2:])
	}
}
