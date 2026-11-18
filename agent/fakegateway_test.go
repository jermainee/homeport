package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// waitFor bounds every wait in these tests.
const waitFor = 10 * time.Second

// testLineID and testObservedIP are what the fake gateway acknowledges.
// testObservedIP is a public address that no other policy rule covers, so a
// refused flow to it proves the own-IP rule.
const testLineID = "line_test_1"

var testObservedIP = netip.MustParseAddr("1.2.3.4")

// fakeGateway is the gateway side of relay-protocol, built from the protocol
// package and quic-go only. It accepts agents, answers their hello and lets a
// test open flows on each connection.
type fakeGateway struct {
	ln        *quic.Listener
	pin       relayprotocol.Pin
	agentCert tls.Certificate
	conns     chan *agentConn

	mu       sync.Mutex
	refuse   int         // handshakes left whose agent certificate is refused
	attempts []time.Time // when each handshake checked the agent's certificate
}

// agentConn is one agent connection that completed the hello.
type agentConn struct {
	conn  *quic.Conn
	hello relayprotocol.Hello
	stats atomic.Pointer[relayprotocol.Stats]
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	gwCert, err := relayprotocol.GenerateSelfSigned("fake-gateway")
	if err != nil {
		t.Fatal(err)
	}
	agentCert, err := relayprotocol.GenerateSelfSigned("homeport-agent")
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGateway{
		pin:       relayprotocol.PinOf(gwCert.Leaf),
		agentCert: agentCert,
		conns:     make(chan *agentConn, 16),
	}
	agentPin := relayprotocol.PinOf(agentCert.Leaf)
	tlsConf := relayprotocol.GatewayTLSConfig(gwCert, func(p relayprotocol.Pin) bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.attempts = append(g.attempts, time.Now())
		if g.refuse > 0 {
			g.refuse--
			return false
		}
		return p == agentPin
	})
	if g.ln, err = quic.ListenAddr("127.0.0.1:0", tlsConf, relayprotocol.GatewayQUICConfig()); err != nil {
		t.Fatal(err)
	}
	go g.acceptLoop()
	t.Cleanup(func() { _ = g.ln.Close() })
	return g
}

// refuseCertificates makes the gateway refuse the agent's certificate in
// its next n handshakes, as a gateway does whose config does not list the
// agent's line yet.
func (g *fakeGateway) refuseCertificates(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refuse = n
}

// handshakes returns when each handshake so far checked the agent's
// certificate, oldest first.
func (g *fakeGateway) handshakes() []time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]time.Time(nil), g.attempts...)
}

func (g *fakeGateway) acceptLoop() {
	for {
		conn, err := g.ln.Accept(context.Background())
		if err != nil {
			return
		}
		go g.handshake(conn)
	}
}

func (g *fakeGateway) handshake(conn *quic.Conn) {
	ctx, cancel := context.WithTimeout(conn.Context(), waitFor)
	defer cancel()
	ctl, err := conn.AcceptStream(ctx)
	if err != nil {
		return
	}
	msg, err := relayprotocol.ReadControl(ctl)
	hello, ok := msg.(relayprotocol.Hello)
	if err != nil || !ok {
		_ = conn.CloseWithError(relayprotocol.CodeProtocolError.ApplicationErrorCode(), "")
		return
	}
	ack := relayprotocol.HelloAck{LineID: testLineID, ObservedIP: testObservedIP}
	if err := relayprotocol.WriteControl(ctl, ack); err != nil {
		return
	}
	ac := &agentConn{conn: conn, hello: hello}
	go func() {
		for {
			m, err := relayprotocol.ReadControl(ctl)
			if errors.Is(err, relayprotocol.ErrUnknownMessage) {
				continue
			}
			if err != nil {
				return
			}
			if s, ok := m.(relayprotocol.Stats); ok {
				ac.stats.Store(&s)
			}
		}
	}()
	g.conns <- ac
}

// nextConn waits for the next agent connection to complete its hello.
func (g *fakeGateway) nextConn(t *testing.T) *agentConn {
	t.Helper()
	select {
	case c := <-g.conns:
		return c
	case <-time.After(waitFor):
		t.Fatal("no agent connected to the gateway")
		return nil
	}
}

// noConnFor fails the test if an agent connects within d.
func (g *fakeGateway) noConnFor(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-g.conns:
		t.Fatal("agent connected although it should not")
	case <-time.After(d):
	}
}

// openFlow opens a flow stream and writes its FlowOpen header.
func (c *agentConn) openFlow(t *testing.T, kind relayprotocol.Kind, dst netip.AddrPort) *quic.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	s, err := c.conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := relayprotocol.WriteFlowOpen(s, relayprotocol.FlowOpen{Kind: kind, Dst: dst}); err != nil {
		t.Fatal(err)
	}
	return s
}

// dial opens a flow and returns the stream with the agent's answer.
func (c *agentConn) dial(t *testing.T, kind relayprotocol.Kind, dst netip.AddrPort) (*quic.Stream, relayprotocol.Status) {
	t.Helper()
	s := c.openFlow(t, kind, dst)
	_ = s.SetReadDeadline(time.Now().Add(waitFor))
	res, err := relayprotocol.ReadFlowResult(s)
	if err != nil {
		t.Fatalf("read flow result: %v", err)
	}
	_ = s.SetReadDeadline(time.Time{})
	return s, res.Status
}

// mustDial is dial for flows that must be accepted.
func (c *agentConn) mustDial(t *testing.T, kind relayprotocol.Kind, dst netip.AddrPort) *quic.Stream {
	t.Helper()
	s, status := c.dial(t, kind, dst)
	if status != relayprotocol.StatusOK {
		t.Fatalf("flow %s answered %s, want OK", kind, status)
	}
	t.Cleanup(func() {
		s.CancelRead(relayprotocol.CodeFlowCancelled.StreamErrorCode())
		s.CancelWrite(relayprotocol.CodeFlowCancelled.StreamErrorCode())
	})
	return s
}

// waitStats waits until the agent's latest stats message satisfies ok.
func (c *agentConn) waitStats(t *testing.T, ok func(relayprotocol.Stats) bool) relayprotocol.Stats {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if s := c.stats.Load(); s != nil && ok(*s) {
			return *s
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := c.stats.Load()
	t.Fatalf("no stats message matched; latest %+v", got)
	return relayprotocol.Stats{}
}

// waitClosed waits until the agent connection ends and returns the error the
// gateway saw.
func (c *agentConn) waitClosed(t *testing.T) error {
	t.Helper()
	select {
	case <-c.conn.Context().Done():
		return context.Cause(c.conn.Context())
	case <-time.After(waitFor):
		t.Fatal("agent connection stayed open")
		return nil
	}
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a logger.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testAgent is a running agent and everything it logged.
type testAgent struct {
	*Agent
	logs *syncBuffer
}

// startAgent runs an agent against g until the test ends. The agent sees
// no VPN and binds no socket unless configure says so.
func startAgent(t *testing.T, g *fakeGateway, configure func(*Config)) *testAgent {
	t.Helper()
	logs := &syncBuffer{}
	cfg := Config{
		GatewayAddr:   g.ln.Addr().String(),
		GatewayPin:    g.pin,
		Certificate:   g.agentCert,
		Logger:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		StatsInterval: 20 * time.Millisecond,
		ReconnectMin:  10 * time.Millisecond,
		ReconnectMax:  50 * time.Millisecond,
		RouteCheck:    func(string) (Route, error) { return Route{}, nil },
	}
	if configure != nil {
		configure(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(waitFor):
			t.Error("Run did not return after its context ended")
		}
	})
	return &testAgent{Agent: a, logs: logs}
}

// allowOrigins lets the agent dial loopback test origins.
func allowOrigins(origins ...netip.AddrPort) func(*Config) {
	return func(c *Config) { c.testOrigins = append(c.testOrigins, origins...) }
}

// startTCPOrigin runs a loopback TCP server that handles each connection
// with handle.
func startTCPOrigin(t *testing.T, handle func(*net.TCPConn)) netip.AddrPort {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.AcceptTCP()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				handle(c)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).AddrPort()
}

// startUDPEcho runs a loopback UDP server that sends every datagram back.
func startUDPEcho(t *testing.T) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, relayprotocol.MaxUDPPayload)
		for {
			n, from, err := pc.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteToUDPAddrPort(buf[:n], from)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).AddrPort()
}

// closedPort returns a loopback TCP address nothing listens on.
func closedPort(t *testing.T) netip.AddrPort {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ap := ln.Addr().(*net.TCPAddr).AddrPort()
	_ = ln.Close()
	return ap
}

// readAll reads s to its end within the test's wait limit.
func readAll(t *testing.T, s *quic.Stream) []byte {
	t.Helper()
	_ = s.SetReadDeadline(time.Now().Add(waitFor))
	b, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return b
}
