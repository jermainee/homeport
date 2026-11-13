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
	tlsConf := relayprotocol.AgentTLSConfig(cert, relayprotocol.AllowPins(gw.RelayPin()))
	conn, err := quic.DialAddr(ctx, gw.QUICAddr().String(), tlsConf, relayprotocol.AgentQUICConfig())
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { conn.CloseWithError(0, "") })
	ctl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	if err := relayprotocol.WriteControl(ctl, relayprotocol.Hello{AgentVersion: "test", OS: runtime.GOOS}); err != nil {
		return nil, err
	}
	msg, err := relayprotocol.ReadControl(ctl)
	if err != nil {
		return nil, err
	}
	ack, ok := msg.(relayprotocol.HelloAck)
	if !ok {
		return nil, errors.New("first control message was not hello_ack")
	}
	a := &fakeAgent{conn: conn, ack: ack, opens: make(chan relayprotocol.FlowOpen, 64)}
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

// relayTo makes the agent connect every TCP flow to origin.
func relayTo(origin string) flowHandler {
	return func(_ relayprotocol.FlowOpen, s *quic.Stream) {
		c, err := net.Dial("tcp", origin)
		if err != nil {
			relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: relayprotocol.StatusRefused})
			s.Close()
			return
		}
		defer c.Close()
		if err := relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: relayprotocol.StatusOK}); err != nil {
			return
		}
		done := make(chan struct{})
		go func() {
			io.Copy(c, s)
			c.(*net.TCPConn).CloseWrite()
			close(done)
		}()
		io.Copy(s, c)
		s.Close()
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
		w.Write([]byte("hello from home"))
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

	if got := string(client.get(t, "http://203.0.113.10/")); got != "hello from home" {
		t.Fatalf("got %q through the agent", got)
	}
	open := <-agent.opens
	if open.Kind != relayprotocol.KindTCP || open.Dst != netip.MustParseAddrPort("203.0.113.10:80") {
		t.Fatalf("agent got %+v, want TCP to 203.0.113.10:80", open)
	}
}

// answer makes the agent answer every flow with status and end it.
func answer(status relayprotocol.Status) flowHandler {
	return func(_ relayprotocol.FlowOpen, s *quic.Stream) {
		relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: status})
		s.Close()
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
				s.Close()
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := c.net.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort("203.0.113.10:80"))
	if err == nil {
		conn.Close()
		t.Fatal("the connection opened; it should have been refused")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("refusal took %v; the client should get a reset, not a timeout", took)
	}
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
		w.Write([]byte("from the new agent"))
	}))
	t.Cleanup(origin.Close)
	f := newRelayFixture(t, answer(relayprotocol.StatusPolicy), "member-user")

	if _, err := startAgent(t, f.gw, f.cert, relayTo(origin.Listener.Addr().String())); err != nil {
		t.Fatalf("second agent: %v", err)
	}

	waitClosed(t, f.agent.conn)
	if got := string(f.client.get(t, "http://203.0.113.10/")); got != "from the new agent" {
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

	reply := f.client.udpRoundTrip(t, netip.MustParseAddrPort("203.0.113.10:7"), []byte("ping"))

	if string(reply) != "ping" {
		t.Fatalf("got %q back", reply)
	}
	open := <-f.agent.opens
	if open.Kind != relayprotocol.KindUDP || open.Dst != netip.MustParseAddrPort("203.0.113.10:7") {
		t.Fatalf("agent got %+v, want UDP to 203.0.113.10:7", open)
	}
}
