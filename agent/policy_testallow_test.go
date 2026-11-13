//go:build e2e

package agent

import (
	"io"
	"net"
	"net/netip"
	"testing"

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
