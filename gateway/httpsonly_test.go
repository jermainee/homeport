package gateway_test

import (
	"context"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
)

// httpsOnlyFixture is a gateway with plain HTTP (port 80), TLS (443), a TCP
// echo (22 and 53) and a UDP echo behind it, and two clients: one whose
// device has HTTPS only and one whose device does not.
type httpsOnlyFixture struct {
	gw            *gateway.Server
	flows         *flowLog
	strict, loose *testClient
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
	if err := f.strict.tryGet("http://11.22.33.44/", 3*time.Second); err == nil {
		t.Error("plain HTTP was allowed")
	}
	// SSH speaks first, in the clear.
	start := time.Now()
	if err := f.strict.echoOver(netip.MustParseAddrPort("11.22.33.44:22"), "SSH-2.0-OpenSSH_9.9\r\n", 5*time.Second); err == nil {
		t.Error("a protocol that is not TLS was allowed")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("refusing a client that speaks first without TLS took %v", took)
	}
	for _, fl := range f.flows.all() {
		if fl.Dst.Port() == 80 || fl.Dst.Port() == 22 {
			t.Errorf("a plain TCP flow was dialed: %+v", fl)
		}
	}
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
