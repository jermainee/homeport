package gateway_test

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
)

// httpsOnlyFixture is a gateway with plain HTTP (port 80), TLS (443), a TCP
// echo (22 and 53) and a UDP echo behind it, and two clients: one whose
// device has HTTPS only and one whose device does not. plain counts the
// bytes that reach the plain origins (80 and 22) from the strict client.
type httpsOnlyFixture struct {
	gw            *gateway.Server
	flows         *flowLog
	strict, loose *testClient
	plain         *countingOrigin
}

// countingOrigin is a TCP server that counts the bytes it receives and
// answers nothing.
type countingOrigin struct {
	addr     string
	received atomic.Int64
}

func newCountingOrigin(t *testing.T) *countingOrigin {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	o := &countingOrigin{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				n, _ := io.Copy(io.Discard, c)
				o.received.Add(n)
				_ = c.Close()
			}()
		}
	}()
	return o
}

func newHTTPSOnlyFixture(t *testing.T, access gateway.AccessControl) *httpsOnlyFixture {
	t.Helper()
	origins := newOrigins(t)
	echo := newHoldingOrigin(t)
	origins.tcp[22] = echo
	origins.tcp[53] = echo
	flows := &flowLog{next: origins}
	gw := newGatewayWith(t, gateway.Config{Dialer: flows, Access: access})
	strict := peerA
	strict.HTTPSOnly = true
	return &httpsOnlyFixture{gw: gw, flows: flows, strict: addPeer(t, gw, strict), loose: addPeer(t, gw, peerB)}
}

// echoOver opens TCP to dst, writes msg and reads it back.
func (c *testClient) echoOver(dst netip.AddrPort, msg string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := c.net.DialContextTCPAddrPort(ctx, dst)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte(msg)); err != nil {
		return err
	}
	_, err = io.ReadFull(conn, make([]byte, len(msg)))
	return err
}

func TestHTTPSOnlyDeviceOpensTLSButNoPlainTCP(t *testing.T) {
	f := newHTTPSOnlyFixture(t, nil)

	if err := f.strict.getTLS("homeport.example", 5*time.Second); err != nil {
		t.Errorf("TLS with a server name: %v", err)
	}
	// A client connecting to an IP address sends no server name.
	if err := f.strict.getTLS("", 5*time.Second); err != nil {
		t.Errorf("TLS without a server name: %v", err)
	}
	f.useCountingOrigins(t)
	if err := f.strict.tryGet("http://11.22.33.44/", 3*time.Second); err == nil {
		t.Error("plain HTTP was allowed")
	}
	// SSH clients send their version first, in the clear.
	start := time.Now()
	if err := f.strict.echoOver(netip.MustParseAddrPort("11.22.33.44:22"), "SSH-2.0-OpenSSH_9.9\r\n", 5*time.Second); err == nil {
		t.Error("a protocol that is not TLS was allowed")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("refusing a client that speaks first without TLS took %v", took)
	}
	// The destinations may be dialed, but not one plain byte reaches them.
	time.Sleep(100 * time.Millisecond)
	if n := f.plain.received.Load(); n != 0 {
		t.Errorf("%d plain bytes reached the destination", n)
	}
}

// useCountingOrigins sends ports 80 and 22 to f.plain from now on.
func (f *httpsOnlyFixture) useCountingOrigins(t *testing.T) {
	t.Helper()
	f.plain = newCountingOrigin(t)
	origins := f.flows.next.(byPort)
	origins.tcp[80] = f.plain.addr
	origins.tcp[22] = f.plain.addr
}

func TestHTTPSOnlyLeavesDNSAndUDPAlone(t *testing.T) {
	f := newHTTPSOnlyFixture(t, nil)

	if got := f.strict.udpRoundTrip(t, netip.MustParseAddrPort("10.77.0.1:53"), []byte("query")); string(got) != "query" {
		t.Errorf("DNS through the virtual resolver answered %q", got)
	}
	// Clients send DNS over TCP to the virtual resolver after a truncated
	// answer; the agent resolves it over HTTPS like any other query.
	if err := f.strict.echoOver(netip.MustParseAddrPort("10.77.0.1:53"), "\x00\x05query", 5*time.Second); err != nil {
		t.Errorf("DNS over TCP to the virtual resolver: %v", err)
	}
	// QUIC, which HTTP/3 runs on, is encrypted UDP.
	if got := f.strict.udpRoundTrip(t, netip.MustParseAddrPort("11.22.33.44:443"), []byte("quic")); string(got) != "quic" {
		t.Errorf("UDP answered %q", got)
	}
}

func TestADeviceWithoutHTTPSOnlyStillOpensPlainTCP(t *testing.T) {
	f := newHTTPSOnlyFixture(t, nil)

	if err := f.loose.tryGet("http://11.22.33.44/", 5*time.Second); err != nil {
		t.Errorf("plain HTTP from a device without HTTPS only: %v", err)
	}
	if err := f.loose.echoOver(netip.MustParseAddrPort("11.22.33.44:22"), "SSH-2.0-OpenSSH_9.9\r\n", 5*time.Second); err != nil {
		t.Errorf("SSH from a device without HTTPS only: %v", err)
	}
}

func TestHTTPSOnlyAppliesUnderFullAccess(t *testing.T) {
	f := newHTTPSOnlyFixture(t, &fixedAccess{access: gateway.AccessFull})

	if err := f.strict.tryGet("http://11.22.33.44/", 3*time.Second); err == nil {
		t.Error("plain HTTP was allowed")
	}
	if err := f.strict.getTLS("homeport.example", 5*time.Second); err != nil {
		t.Errorf("TLS: %v", err)
	}
	if err := f.loose.tryGet("http://11.22.33.44/", 5*time.Second); err != nil {
		t.Errorf("plain HTTP from a device without HTTPS only: %v", err)
	}
}

func TestHTTPSOnlyRefusesAnUnreachableDestinationAtTheSYN(t *testing.T) {
	// As for any device, a destination the line cannot reach is refused
	// before the client's handshake completes, so a browser's Happy
	// Eyeballs falls back to another address instead of committing to a
	// connection that then dies.
	strict := peerA
	strict.HTTPSOnly = true
	gw := newGateway(t, refuseAll{})
	a := addPeer(t, gw, strict)

	dialRefusedQuickly(t, a, netip.MustParseAddrPort("[2a00:1450:4001::1]:443"))
	dialRefusedQuickly(t, a, netip.MustParseAddrPort("11.22.33.44:443"))
}

func TestHTTPSOnlyRefusesAServerThatSpeaksFirstAtOnce(t *testing.T) {
	// SMTP, IMAP and FTP servers greet first. TLS never does, so the flow
	// is refused as soon as the server speaks, not after the client's
	// ClientHello timeout.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("220 mail.example ESMTP ready\r\n"))
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	strict := peerA
	strict.HTTPSOnly = true
	gw := newGateway(t, toOrigin{tcp: ln.Addr().String(), udp: newUDPEcho(t)})
	a := addPeer(t, gw, strict)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := a.net.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort("11.22.33.44:587"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(make([]byte, 64))
	if err == nil {
		t.Errorf("the client got %d bytes of a server that speaks first in the clear", n)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the refusal took %v; it should come as soon as the server speaks", took)
	}
}

func TestHTTPSOnlyIdleConnectionsDoNotBlockNewOnes(t *testing.T) {
	// Connections whose client has not sent its ClientHello yet, such as a
	// browser's preconnects, must not hold the device's pending-dial slots.
	strict := peerA
	strict.HTTPSOnly = true
	origins := newOrigins(t)
	gw := newGatewayWith(t, gateway.Config{Dialer: origins, MaxPendingFlowsPerPeer: 2})
	a := addPeer(t, gw, strict)
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := a.net.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort("11.22.33.44:443"))
		cancel()
		if err != nil {
			t.Fatalf("idle connection: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
	}

	if err := a.getTLS("homeport.example", 5*time.Second); err != nil {
		t.Errorf("TLS while three connections sit idle: %v", err)
	}
}

func TestTurningHTTPSOnlyOnResetsPlainFlowsButKeepsTheTunnel(t *testing.T) {
	gw := newGateway(t, toOrigin{tcp: newHoldingOrigin(t), udp: newUDPEcho(t)})
	key := newKey(t)
	p := peerA
	p.PublicKey = key.PublicKey()
	if err := gw.AddPeer(p); err != nil {
		t.Fatal(err)
	}
	a := newClient(t, gw, key, p.IPv4, p.IPv6)
	held := a.dialHeld(t) // a plain TCP flow

	gw.SetPeerHTTPSOnly(p.PublicKey, true)

	assertConnEnds(t, held)
	if err := a.echoOver(netip.MustParseAddrPort("11.22.33.44:80"), "plain", 2*time.Second); err == nil {
		t.Error("a new plain flow was allowed after HTTPS only was turned on")
	}
	// The WireGuard session stays: DNS works at once, without a new handshake.
	if got := a.udpRoundTrip(t, netip.MustParseAddrPort("10.77.0.1:53"), []byte("q")); string(got) != "q" {
		t.Errorf("DNS after turning HTTPS only on answered %q", got)
	}
	gw.SetPeerHTTPSOnly(p.PublicKey, false)
	if err := a.echoOver(netip.MustParseAddrPort("11.22.33.44:80"), "plain", 5*time.Second); err != nil {
		t.Errorf("plain TCP after turning HTTPS only off: %v", err)
	}
}
