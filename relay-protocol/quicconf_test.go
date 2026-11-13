package relayprotocol_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	rp "github.com/jermainee/homeport/relay-protocol"
)

func TestQUICConfigsKeepNATMappingsAlive(t *testing.T) {
	for name, cfg := range map[string]*quic.Config{
		"agent":   rp.AgentQUICConfig(),
		"gateway": rp.GatewayQUICConfig(),
	} {
		if cfg.KeepAlivePeriod != 15*time.Second {
			t.Errorf("%s KeepAlivePeriod = %v, want 15s", name, cfg.KeepAlivePeriod)
		}
		if cfg.MaxIdleTimeout < 45*time.Second || cfg.MaxIdleTimeout > 60*time.Second {
			t.Errorf("%s MaxIdleTimeout = %v, want 45–60s", name, cfg.MaxIdleTimeout)
		}
	}
}

func TestConfigBuildersReturnFreshValues(t *testing.T) {
	a, b := rp.AgentQUICConfig(), rp.AgentQUICConfig()
	a.MaxIdleTimeout = time.Hour
	if b.MaxIdleTimeout == time.Hour {
		t.Fatal("AgentQUICConfig returned a shared value")
	}
}

// pair is an agent and a gateway, each with its own identity.
type pair struct {
	agent, gateway tls.Certificate
}

func newPair(t *testing.T) pair {
	return pair{agent: mustGenerate(t, "agent"), gateway: mustGenerate(t, "gateway")}
}

func (p pair) agentTLS() *tls.Config {
	return rp.AgentTLSConfig(p.agent, rp.AllowPins(rp.PinOf(p.gateway.Leaf)))
}

func (p pair) gatewayTLS() *tls.Config {
	return rp.GatewayTLSConfig(p.gateway, rp.AllowPins(rp.PinOf(p.agent.Leaf)))
}

// handshakeTCP runs a TLS handshake between the two configs over loopback
// TCP and returns each side's error.
func handshakeTCP(t *testing.T, agentCfg, gatewayCfg *tls.Config) (agentErr, gatewayErr error, state tls.ConnectionState) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	type result struct {
		err   error
		state tls.ConnectionState
	}
	done := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- result{err: err}
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		s := tls.Server(c, gatewayCfg)
		err = s.Handshake()
		done <- result{err: err, state: s.ConnectionState()}
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	client := tls.Client(c, agentCfg)
	agentErr = client.Handshake()
	if agentErr == nil {
		// In TLS 1.3 the client finishes before the server has checked its
		// certificate; a rejection arrives as an alert on the first read.
		_, agentErr = client.Read(make([]byte, 1))
		if errors.Is(agentErr, io.EOF) {
			agentErr = nil
		}
	}
	r := <-done
	return agentErr, r.err, r.state
}

func TestTLSHandshakeWithPinnedCertificates(t *testing.T) {
	p := newPair(t)
	agentErr, gatewayErr, state := handshakeTCP(t, p.agentTLS(), p.gatewayTLS())
	if agentErr != nil || gatewayErr != nil {
		t.Fatalf("handshake failed: agent %v, gateway %v", agentErr, gatewayErr)
	}
	if state.NegotiatedProtocol != rp.ALPN {
		t.Fatalf("ALPN = %q, want %q", state.NegotiatedProtocol, rp.ALPN)
	}
	if state.Version != tls.VersionTLS13 {
		t.Fatalf("TLS version = %#x, want TLS 1.3", state.Version)
	}
	pin, err := rp.PeerPin(state)
	if err != nil {
		t.Fatalf("PeerPin: %v", err)
	}
	if pin != rp.PinOf(p.agent.Leaf) {
		t.Fatalf("gateway sees pin %s, want the agent's %s", pin, rp.PinOf(p.agent.Leaf))
	}
}

func TestAgentRejectsGatewayWithAnotherKey(t *testing.T) {
	p := newPair(t)
	impostor := p
	impostor.gateway = mustGenerate(t, "gateway")

	agentErr, _, _ := handshakeTCP(t, p.agentTLS(), impostor.gatewayTLS())
	if !errors.Is(agentErr, rp.ErrPinMismatch) {
		t.Fatalf("agent error = %v, want ErrPinMismatch", agentErr)
	}
}

func TestGatewayRejectsUnknownAgent(t *testing.T) {
	p := newPair(t)
	stranger := p
	stranger.agent = mustGenerate(t, "agent")

	agentErr, gatewayErr, _ := handshakeTCP(t, stranger.agentTLS(), p.gatewayTLS())
	if !errors.Is(gatewayErr, rp.ErrPinMismatch) {
		t.Fatalf("gateway error = %v, want ErrPinMismatch", gatewayErr)
	}
	if agentErr == nil {
		t.Fatal("agent did not notice the rejection")
	}
}

func TestGatewayRejectsAgentWithoutCertificate(t *testing.T) {
	p := newPair(t)
	anonymous := p.agentTLS()
	anonymous.Certificates = nil

	_, gatewayErr, _ := handshakeTCP(t, anonymous, p.gatewayTLS())
	if gatewayErr == nil {
		t.Fatal("gateway accepted an agent without a certificate")
	}
}

func TestTLSConfigsDisableSessionResumption(t *testing.T) {
	// VerifyPeerCertificate does not run on resumed sessions, so a revoked
	// pin could otherwise come back with an old ticket.
	p := newPair(t)
	if !p.agentTLS().SessionTicketsDisabled || !p.gatewayTLS().SessionTicketsDisabled {
		t.Fatal("session tickets are enabled")
	}
	if p.agentTLS().ClientSessionCache != nil {
		t.Fatal("agent has a client session cache")
	}
}

// quicPair connects an agent to a gateway over loopback QUIC.
func quicPair(t *testing.T) (agent, gateway *quic.Conn, p pair) {
	t.Helper()
	p = newPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	ln, err := quic.ListenAddr("127.0.0.1:0", p.gatewayTLS(), rp.GatewayQUICConfig())
	if err != nil {
		t.Fatalf("ListenAddr: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	accepted := make(chan *quic.Conn, 1)
	go func() {
		c, err := ln.Accept(ctx)
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	agent, err = quic.DialAddr(ctx, ln.Addr().String(), p.agentTLS(), rp.AgentQUICConfig())
	if err != nil {
		t.Fatalf("DialAddr: %v", err)
	}
	t.Cleanup(func() { agent.CloseWithError(rp.CodeNoError.ApplicationErrorCode(), "") })
	gateway, ok := <-accepted
	if !ok {
		t.Fatal("gateway did not accept the agent")
	}
	t.Cleanup(func() { gateway.CloseWithError(rp.CodeNoError.ApplicationErrorCode(), "") })
	return agent, gateway, p
}

func TestQUICConnectionUsesRelayALPNAndPins(t *testing.T) {
	agent, gateway, p := quicPair(t)
	if got := agent.ConnectionState().TLS.NegotiatedProtocol; got != rp.ALPN {
		t.Fatalf("agent ALPN = %q, want %q", got, rp.ALPN)
	}
	pin, err := rp.PeerPin(gateway.ConnectionState().TLS)
	if err != nil {
		t.Fatalf("PeerPin: %v", err)
	}
	if pin != rp.PinOf(p.agent.Leaf) {
		t.Fatal("gateway sees another agent pin")
	}
}

func TestQUICControlStreamAndFlowStream(t *testing.T) {
	agent, gateway, _ := quicPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The agent opens the control stream and says hello.
	control, err := agent.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("agent OpenStreamSync: %v", err)
	}
	if err := rp.WriteControl(control, rp.Hello{AgentVersion: "0.1.0", OS: "darwin"}); err != nil {
		t.Fatalf("WriteControl hello: %v", err)
	}
	gwControl, err := gateway.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("gateway AcceptStream: %v", err)
	}
	if msg, err := rp.ReadControl(gwControl); err != nil || msg != (rp.Hello{AgentVersion: "0.1.0", OS: "darwin"}) {
		t.Fatalf("gateway read %#v, %v", msg, err)
	}
	ack := rp.HelloAck{LineID: "line_1", ObservedIP: netip.MustParseAddr("198.51.100.23")}
	if err := rp.WriteControl(gwControl, ack); err != nil {
		t.Fatalf("WriteControl hello_ack: %v", err)
	}
	if msg, err := rp.ReadControl(control); err != nil || msg != ack {
		t.Fatalf("agent read %#v, %v", msg, err)
	}

	// The gateway opens a TCP flow and pipelines the first bytes.
	flow, err := gateway.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("gateway OpenStreamSync: %v", err)
	}
	open := rp.FlowOpen{Kind: rp.KindTCP, Dst: netip.MustParseAddrPort("203.0.113.7:443")}
	if err := rp.WriteFlowOpen(flow, open); err != nil {
		t.Fatalf("WriteFlowOpen: %v", err)
	}
	if _, err := flow.Write([]byte("ping")); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	agentFlow, err := agent.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("agent AcceptStream: %v", err)
	}
	if got, err := rp.ReadFlowOpen(agentFlow); err != nil || got != open {
		t.Fatalf("agent read FlowOpen %+v, %v", got, err)
	}
	if err := rp.WriteFlowResult(agentFlow, rp.FlowResult{Status: rp.StatusOK}); err != nil {
		t.Fatalf("WriteFlowResult: %v", err)
	}
	payload := make([]byte, 4)
	if _, err := io.ReadFull(agentFlow, payload); err != nil || string(payload) != "ping" {
		t.Fatalf("agent read payload %q, %v", payload, err)
	}
	if _, err := agentFlow.Write([]byte("pong")); err != nil {
		t.Fatalf("agent write: %v", err)
	}

	if res, err := rp.ReadFlowResult(flow); err != nil || res.Status != rp.StatusOK {
		t.Fatalf("gateway read FlowResult %+v, %v", res, err)
	}
	if _, err := io.ReadFull(flow, payload); err != nil || string(payload) != "pong" {
		t.Fatalf("gateway read payload %q, %v", payload, err)
	}
}

func TestQUICGatewayCanOpen1024ConcurrentFlows(t *testing.T) {
	_, gateway, _ := quicPair(t)
	for i := range 1024 {
		if _, err := gateway.OpenStream(); err != nil {
			t.Fatalf("OpenStream #%d: %v", i+1, err)
		}
	}
}

func TestQUICAgentCanOpenOnlyItsControlStream(t *testing.T) {
	agent, _, _ := quicPair(t)
	if _, err := agent.OpenStream(); err != nil {
		t.Fatalf("control stream: %v", err)
	}
	if _, err := agent.OpenStream(); err == nil {
		t.Fatal("agent opened a second stream to the gateway")
	}
	if _, err := agent.OpenUniStream(); err == nil {
		t.Fatal("agent opened a unidirectional stream to the gateway")
	}
}

func TestQUICFlowResetCarriesRelayCode(t *testing.T) {
	agent, gateway, _ := quicPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	flow, err := gateway.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}
	open := rp.FlowOpen{Kind: rp.KindTCP, Dst: netip.MustParseAddrPort("203.0.113.7:443")}
	if err := rp.WriteFlowOpen(flow, open); err != nil {
		t.Fatalf("WriteFlowOpen: %v", err)
	}
	agentFlow, err := agent.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	if _, err := rp.ReadFlowOpen(agentFlow); err != nil {
		t.Fatalf("ReadFlowOpen: %v", err)
	}
	// The destination reset the TCP connection.
	agentFlow.CancelWrite(rp.CodeFlowReset.StreamErrorCode())

	_, err = flow.Read(make([]byte, 1))
	var streamErr *quic.StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("gateway read error = %v, want a *quic.StreamError", err)
	}
	if !streamErr.Remote || rp.Code(streamErr.ErrorCode) != rp.CodeFlowReset {
		t.Fatalf("stream error = %+v, want remote %v", streamErr, rp.CodeFlowReset)
	}
}

func TestCodesHaveDistinctValuesAndNames(t *testing.T) {
	codes := []rp.Code{
		rp.CodeNoError, rp.CodeProtocolError, rp.CodeUnsupportedVersion, rp.CodeInternalError,
		rp.CodeUnauthorized, rp.CodeGoingAway, rp.CodeFlowReset, rp.CodeFlowCancelled,
	}
	values := map[rp.Code]bool{}
	names := map[string]bool{}
	for _, c := range codes {
		if values[c] || names[c.String()] {
			t.Fatalf("code %d (%s) is not unique", uint64(c), c)
		}
		values[c], names[c.String()] = true, true
	}
	if got := rp.Code(0xbeef).String(); got != "Code(0xbeef)" {
		t.Fatalf("unknown code String() = %q", got)
	}
}
