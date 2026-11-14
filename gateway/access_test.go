package gateway_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
)

// fixedAccess gives every peer the same access and records who asked.
type fixedAccess struct {
	mu     sync.Mutex
	access gateway.Access
	hosts  map[string]bool
	asked  []gateway.Peer
}

func (a *fixedAccess) Access(_ context.Context, p gateway.Peer) gateway.Access {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, p)
	return a.access
}

func (a *fixedAccess) TopUpHost(h string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hosts[h]
}

// accessFixture is a gateway with an AccessControl, a TLS and a plain HTTP
// origin behind it, and one connected client.
type accessFixture struct {
	gw     *gateway.Server
	access *fixedAccess
	client *testClient
	key    gateway.Key
	peer   gateway.Peer
}

func newAccessFixture(t *testing.T, access gateway.Access, dialer gateway.FlowDialer) *accessFixture {
	t.Helper()
	acc := &fixedAccess{access: access, hosts: map[string]bool{"homeport.example": true}}
	gw := newGatewayWith(t, gateway.Config{Dialer: dialer, Access: acc})
	key := newKey(t)
	peer := gateway.Peer{ID: "peer-1", PublicKey: key.PublicKey(), IPv4: netip.MustParseAddr("10.77.0.2")}
	if err := gw.AddPeer(peer); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	return &accessFixture{gw: gw, access: acc, client: newClient(t, gw, key, peer.IPv4), key: key, peer: peer}
}

// byPort sends TCP flows to an origin chosen by destination port, and UDP
// flows to udp.
type byPort struct {
	tcp map[uint16]string
	udp string
}

func (d byPort) DialTCP(ctx context.Context, f gateway.Flow) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.tcp[f.Dst.Port()])
}

func (d byPort) DialUDP(ctx context.Context, _ gateway.Flow) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "udp", d.udp)
}

func newOrigins(t *testing.T) byPort {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	plain := httptest.NewServer(handler)
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(handler)
	t.Cleanup(secure.Close)
	return byPort{
		tcp: map[uint16]string{80: plain.Listener.Addr().String(), 443: secure.Listener.Addr().String()},
		udp: newUDPEcho(t),
	}
}

// getTLS fetches https://11.22.33.44/ through the tunnel, naming host in
// the ClientHello.
func (c *testClient) getTLS(host string, timeout time.Duration) error {
	httpClient := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:       c.net.DialContext,
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{ServerName: host, InsecureSkipVerify: true}, // #nosec G402 -- test origin
		},
	}
	resp, err := httpClient.Get("https://11.22.33.44/")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.ReadAll(resp.Body)
	return err
}

func TestAccessControlIsAskedForEachFlowWithThePeer(t *testing.T) {
	f := newAccessFixture(t, gateway.AccessFull, newOrigins(t))

	if err := f.client.tryGet("http://11.22.33.44/", 5*time.Second); err != nil {
		t.Fatalf("GET with full access: %v", err)
	}

	f.access.mu.Lock()
	defer f.access.mu.Unlock()
	if len(f.access.asked) == 0 || f.access.asked[0] != f.peer {
		t.Errorf("access control was asked for %+v, want %+v", f.access.asked, f.peer)
	}
}

func TestNoAccessRefusesEveryFlow(t *testing.T) {
	f := newAccessFixture(t, gateway.AccessNone, newOrigins(t))
	assertResetQuickly(t, f.client)
}

func TestTopUpAccessAllowsOnlyDNSAndTLSToTheTopUpHosts(t *testing.T) {
	f := newAccessFixture(t, gateway.AccessTopUp, newOrigins(t))

	if err := f.client.getTLS("homeport.example", 5*time.Second); err != nil {
		t.Errorf("TLS to a top-up host: %v", err)
	}
	if err := f.client.getTLS("other.example", 3*time.Second); err == nil {
		t.Error("TLS to another host was allowed")
	}
	if err := f.client.tryGet("http://11.22.33.44/", 3*time.Second); err == nil {
		t.Error("plain HTTP was allowed")
	}
	if got := f.client.udpRoundTrip(t, netip.MustParseAddrPort("10.77.0.1:53"), []byte("query")); string(got) != "query" {
		t.Errorf("DNS through the virtual resolver answered %q", got)
	}
	if err := f.client.tryUDP(netip.MustParseAddrPort("11.22.33.44:53"), time.Second); err == nil {
		t.Error("DNS to another resolver was allowed")
	}
}

func TestResetPeerFlowsEndsOpenConnections(t *testing.T) {
	d := &recordingDialer{target: newHoldingOrigin(t)}
	f := newAccessFixture(t, gateway.AccessFull, d)
	conn := f.client.dialHeld(t)

	f.gw.ResetPeerFlows(f.peer.PublicKey)

	assertConnEnds(t, conn)
	waitUntil(t, d.allClosed)
}

func TestRemovedPeersOpenConnectionsAreClosed(t *testing.T) {
	d := &recordingDialer{target: newHoldingOrigin(t)}
	gw := newGateway(t, d)
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	newClient(t, gw, key, peerIP).dialHeld(t)

	if _, err := gw.RemovePeer(key.PublicKey()); err != nil {
		t.Fatalf("remove peer: %v", err)
	}

	waitUntil(t, d.allClosed)
}

func TestSuspendedPeerIsCutOffAndNotMetered(t *testing.T) {
	d := &recordingDialer{target: newHoldingOrigin(t)}
	f := newAccessFixture(t, gateway.AccessFull, d)
	f.client.dialHeld(t)

	f.gw.SuspendPeer(f.peer.PublicKey, true)

	waitUntil(t, d.allClosed)
	before := waitStable(t, 200*time.Millisecond, func() gateway.Usage { return f.gw.PeerUsage(f.peer.IPv4) })
	if err := f.client.tryUDP(netip.MustParseAddrPort("10.77.0.1:53"), 500*time.Millisecond); err == nil {
		t.Error("a suspended peer's packet got an answer")
	}
	if after := f.gw.PeerUsage(f.peer.IPv4); after != before {
		t.Errorf("usage moved from %+v to %+v while the peer was suspended", before, after)
	}

	f.gw.SuspendPeer(f.peer.PublicKey, false)

	f.client.dialHeld(t)
}

// newHoldingOrigin is a TCP echo server that keeps every connection open
// until the other side closes it.
func newHoldingOrigin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// dialHeld opens a TCP connection through the tunnel to an echo origin and
// waits for the echo, so the gateway is relaying the flow.
func (c *testClient) dialHeld(t *testing.T) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := c.net.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort("11.22.33.44:80"))
	if err != nil {
		t.Fatalf("dial through the tunnel: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 5)); err != nil {
		t.Fatalf("read the echo: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn
}

// tryUDP sends one datagram and reports whether an answer came back in time.
func (c *testClient) tryUDP(dst netip.AddrPort, timeout time.Duration) error {
	return c.tryUDPFrom(netip.Addr{}, dst, timeout)
}

// tryUDPFrom is tryUDP from the client's address src, or from any of its
// addresses if src is the zero Addr.
func (c *testClient) tryUDPFrom(src netip.Addr, dst netip.AddrPort, timeout time.Duration) error {
	var laddr netip.AddrPort
	if src.IsValid() {
		laddr = netip.AddrPortFrom(src, 0)
	}
	uc, err := c.net.DialUDPAddrPort(laddr, dst)
	if err != nil {
		return err
	}
	defer func() { _ = uc.Close() }()
	if _, err := uc.Write([]byte("ping")); err != nil {
		return err
	}
	_ = uc.SetReadDeadline(time.Now().Add(timeout))
	_, err = uc.Read(make([]byte, 64))
	return err
}

// assertConnEnds checks that the gateway ended a connection the client held
// open: a read fails instead of blocking.
func assertConnEnds(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Read(make([]byte, 1))
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatal("the connection is still open")
	}
}

// resettingAccess resets the peer's flows once, while it decides its first
// answer, as a session does when it changes state, then answers later
// calls with access.
type resettingAccess struct {
	fixedAccess
	gw    *gateway.Server
	first gateway.Access
	once  sync.Once
}

func (a *resettingAccess) Access(ctx context.Context, p gateway.Peer) gateway.Access {
	answer := a.fixedAccess.Access(ctx, p)
	a.once.Do(func() {
		a.gw.ResetPeerFlows(p.PublicKey)
		answer = a.first
	})
	return answer
}

func TestFlowAdmittedAfterAResetDuringTheDecisionStaysOpen(t *testing.T) {
	// The session's first lease was refused: it went top-up only, resetting
	// the peer's flows, and this flow is admitted top-up only.
	acc := &resettingAccess{fixedAccess: fixedAccess{access: gateway.AccessTopUp}, first: gateway.AccessTopUp}
	gw := newGatewayWith(t, gateway.Config{Dialer: newOrigins(t), Access: acc})
	acc.gw = gw
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{ID: "peer-1", PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}

	got := newClient(t, gw, key, peerIP).udpRoundTrip(t, netip.MustParseAddrPort("10.77.0.1:53"), []byte("query"))

	if string(got) != "query" {
		t.Errorf("DNS answered %q", got)
	}
}

func TestFlowIsDecidedAgainWhenAResetRacesItsDecision(t *testing.T) {
	// The access check answered full, but the session went top-up only
	// (resetting the peer's flows) before the answer was used.
	acc := &resettingAccess{fixedAccess: fixedAccess{access: gateway.AccessTopUp}, first: gateway.AccessFull}
	gw := newGatewayWith(t, gateway.Config{Dialer: newOrigins(t), Access: acc})
	acc.gw = gw
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{ID: "peer-1", PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}

	if err := newClient(t, gw, key, peerIP).tryGet("http://11.22.33.44/", 3*time.Second); err == nil {
		t.Error("plain HTTP went through on a full-access answer that a reset had overtaken")
	}
}
