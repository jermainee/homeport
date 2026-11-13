//go:build e2e

package agent

import (
	"io"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

func TestE2EBuildsMayAllowALoopbackTestOrigin(t *testing.T) {
	origin := startTCPOrigin(t, func(c *net.TCPConn) { _, _ = io.Copy(c, c) })
	g := newFakeGateway(t)
	startAgent(t, g, func(c *Config) { AllowTestOrigin(c, origin) })

	s := g.nextConn(t).mustDial(t, relayprotocol.KindTCP, origin)
	_, _ = s.Write([]byte("echo"))
	_ = s.Close()
	if got := string(readAll(t, s)); got != "echo" {
		t.Fatalf("echo = %q", got)
	}
}

// testDst is a public-looking destination the e2e tests send through a real
// gateway, which drops packets addressed to loopback.
var testDst = netip.MustParseAddrPort("203.0.113.10:80")

func TestE2EBuildsMayRouteADestinationToALoopbackTestOrigin(t *testing.T) {
	origin := startTCPOrigin(t, func(c *net.TCPConn) { _, _ = io.Copy(c, c) })
	g := newFakeGateway(t)
	startAgent(t, g, func(c *Config) { RouteTestOrigin(c, testDst, origin) })

	s := g.nextConn(t).mustDial(t, relayprotocol.KindTCP, testDst)
	_, _ = s.Write([]byte("echo"))
	_ = s.Close()
	if got := string(readAll(t, s)); got != "echo" {
		t.Fatalf("echo = %q", got)
	}
}

func TestARouteCoversOnlyItsOwnPort(t *testing.T) {
	origin := startUDPEcho(t)
	dst := netip.AddrPortFrom(testDst.Addr(), 7)
	g := newFakeGateway(t)
	startAgent(t, g, func(c *Config) { RouteTestOrigin(c, dst, origin) })
	conn := g.nextConn(t)

	s := conn.mustDial(t, relayprotocol.KindUDP, dst)
	if err := relayprotocol.WriteUDPFrame(s, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = s.SetReadDeadline(time.Now().Add(waitFor))
	if got, err := relayprotocol.ReadUDPFrame(s, nil); err != nil || string(got) != "ping" {
		t.Fatalf("echo = %q, %v", got, err)
	}
	if _, status := conn.dial(t, relayprotocol.KindUDP, netip.AddrPortFrom(dst.Addr(), 8)); status != relayprotocol.StatusPolicy {
		t.Fatalf("another port of the routed address answered %s, want POLICY", status)
	}
}

func TestDNSAnswersMayNameARoutedDestination(t *testing.T) {
	origin := startTCPOrigin(t, func(*net.TCPConn) {})
	unrouted := netip.MustParseAddr("203.0.113.11")
	res := &fakeResolver{answers: map[string][]netip.Addr{
		"ip4 origin.homeport.test.": {testDst.Addr(), unrouted},
	}}
	g := newFakeGateway(t)
	startAgent(t, g, func(c *Config) {
		c.Resolver = res
		RouteTestOrigin(c, testDst, origin)
	})
	s := g.nextConn(t).mustDial(t, relayprotocol.KindDNS, virtualResolver)

	got := exchange(t, s, query(t, 1, "origin.homeport.test.", dnsmessage.TypeA))
	if addrs := answerAddrs(got[1]); !slices.Equal(addrs, []netip.Addr{testDst.Addr()}) {
		t.Fatalf("answers = %v, want only the routed address", addrs)
	}
}

func TestTestOriginsMustBeLoopback(t *testing.T) {
	cert, err := relayprotocol.GenerateSelfSigned("agent")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{GatewayAddr: "gateway.test:443", GatewayPin: relayprotocol.PinOf(cert.Leaf), Certificate: cert}
	AllowTestOrigin(&cfg, netip.MustParseAddrPort("192.168.1.1:80"))
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted a test origin in the home network")
	}
}

func TestRoutedTestOriginsMustBeLoopback(t *testing.T) {
	cert, err := relayprotocol.GenerateSelfSigned("agent")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{GatewayAddr: "gateway.test:443", GatewayPin: relayprotocol.PinOf(cert.Leaf), Certificate: cert}
	RouteTestOrigin(&cfg, testDst, netip.MustParseAddrPort("192.168.1.1:80"))
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted a route into the home network")
	}
}

func TestRoutesAddedToACopyStayInTheCopy(t *testing.T) {
	var base Config
	for port := uint16(8000); port < 8003; port++ { // leaves spare capacity in the slice
		RouteTestOrigin(&base, netip.AddrPortFrom(testDst.Addr(), port), netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port))
	}
	other := base
	RouteTestOrigin(&other, netip.MustParseAddrPort("203.0.113.11:80"), netip.MustParseAddrPort("127.0.0.1:8081"))
	if len(base.testRoutes) != 3 {
		t.Fatalf("base has %d routes after a route was added to its copy, want 3", len(base.testRoutes))
	}
	RouteTestOrigin(&base, netip.MustParseAddrPort("203.0.113.12:80"), netip.MustParseAddrPort("127.0.0.1:8082"))
	if !slices.Contains(other.testOrigins, netip.MustParseAddrPort("127.0.0.1:8081")) {
		t.Fatalf("a route added to base overwrote its copy's origin: %v", other.testOrigins)
	}
}
