package agent

import (
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

func TestLogsNeverNameADestination(t *testing.T) {
	tcpOrigin := startTCPOrigin(t, func(c *net.TCPConn) {
		b, _ := io.ReadAll(c)
		_, _ = c.Write(b)
	})
	udpOrigin := startUDPEcho(t)
	refusing := closedPort(t)
	const secretName = "secret-destination.homeport.test."
	secretAnswer := netip.MustParseAddr("11.22.33.44")
	g := newFakeGateway(t)
	a := startAgent(t, g, func(c *Config) {
		allowOrigins(tcpOrigin, udpOrigin, refusing)(c)
		c.Resolver = &fakeResolver{answers: map[string][]netip.Addr{
			"ip4 " + secretName: {secretAnswer},
		}}
	})
	c := g.nextConn(t)

	// Every kind of flow and outcome, at debug level.
	s := c.mustDial(t, relayprotocol.KindTCP, tcpOrigin)
	_, _ = s.Write([]byte("hello"))
	_ = s.Close()
	readAll(t, s)

	u := c.mustDial(t, relayprotocol.KindUDP, udpOrigin)
	_ = relayprotocol.WriteUDPFrame(u, []byte("ping"))
	_, _ = relayprotocol.ReadUDPFrame(u, nil)
	_ = u.Close()

	d := c.mustDial(t, relayprotocol.KindDNS, virtualResolver)
	exchange(t, d, query(t, 1, secretName, dnsmessage.TypeA))
	_ = d.Close()

	c.dial(t, relayprotocol.KindTCP, refusing)
	c.dial(t, relayprotocol.KindTCP, netip.MustParseAddrPort("192.168.178.1:80"))
	c.dial(t, relayprotocol.KindUDP, netip.MustParseAddrPort("[fd00:dead:beef::1]:53"))
	c.dial(t, relayprotocol.KindTCP, netip.MustParseAddrPort("11.22.33.44:25"))
	c.waitStats(t, func(st relayprotocol.Stats) bool { return st.FlowsTotal == 3 && st.FlowsActive == 0 })

	// And a reconnect and a pause.
	_ = c.conn.CloseWithError(relayprotocol.CodeInternalError.ApplicationErrorCode(), "")
	g.nextConn(t)
	a.Pause()
	waitState(t, a.Agent, StatePaused)

	logs := a.logs.String()
	for _, want := range []string{"connected to gateway", "flow refused by policy", "disconnected from gateway"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs lack %q, so this test checks nothing; logs:\n%s", want, logs)
		}
	}
	forbidden := []string{
		secretName, strings.TrimSuffix(secretName, "."), secretAnswer.String(),
		"192.168.178.1", "fd00:dead:beef::1", testObservedIP.String(),
	}
	for _, ap := range []netip.AddrPort{tcpOrigin, udpOrigin, refusing} {
		port := strconv.Itoa(int(ap.Port()))
		forbidden = append(forbidden, ap.String(), ":"+port, "="+port, " "+port)
	}
	for _, f := range forbidden {
		if strings.Contains(logs, f) {
			t.Errorf("logs contain the destination %q:\n%s", f, logs)
		}
	}
}
