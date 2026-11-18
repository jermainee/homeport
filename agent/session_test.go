package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/net/dns/dnsmessage"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// quietPeriod is how long a test waits to show that an agent stays away.
// Reconnects in these tests take at most ReconnectMax (50 ms).
const quietPeriod = 300 * time.Millisecond

func TestAgentReconnectsAfterTheGatewayKillsTheConnection(t *testing.T) {
	g := newFakeGateway(t)
	a := startAgent(t, g, nil)

	first := g.nextConn(t)
	_ = first.conn.CloseWithError(relayprotocol.CodeInternalError.ApplicationErrorCode(), "")

	second := g.nextConn(t)
	if second.conn == first.conn {
		t.Fatal("got the killed connection again")
	}
	waitState(t, a.Agent, StateConnected)
}

func TestPauseClosesTheConnectionUntilResume(t *testing.T) {
	g := newFakeGateway(t)
	a := startAgent(t, g, nil)
	c := g.nextConn(t)

	a.Pause()
	assertGoingAway(t, c.waitClosed(t))
	waitState(t, a.Agent, StatePaused)
	if !a.Stats().Paused {
		t.Fatal("Stats().Paused = false after Pause")
	}
	g.noConnFor(t, quietPeriod)

	a.Resume()
	g.nextConn(t)
	waitState(t, a.Agent, StateConnected)
}

var (
	homeEn0  = Interface{Index: 4, Name: "en0"}
	homeEn10 = Interface{Index: 21, Name: "en10"}
)

// fakeRoute is a route check whose answer a test changes.
type fakeRoute struct{ r atomic.Pointer[Route] }

func newFakeRoute(r Route) *fakeRoute {
	f := &fakeRoute{}
	f.set(r)
	return f
}

func (f *fakeRoute) set(r Route) { f.r.Store(&r) }

// configure makes the agent check f every 5 ms.
func (f *fakeRoute) configure(c *Config) {
	c.RouteCheck = func(string) (Route, error) { return *f.r.Load(), nil }
	c.RouteCheckInterval = 5 * time.Millisecond
}

// boundSocket is a socket the agent bound: its network, its destination
// and the interface it was bound to.
type boundSocket struct {
	network string
	dst     netip.Addr
	home    string
}

// bindLog records the sockets the agent binds instead of binding them.
type bindLog struct {
	mu      sync.Mutex
	sockets []boundSocket
}

func (l *bindLog) configure(c *Config) {
	c.bind = func(_ syscall.RawConn, network string, dst netip.Addr, home Interface) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.sockets = append(l.sockets, boundSocket{network, dst, home.Name})
		return nil
	}
}

func (l *bindLog) all() []boundSocket {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.sockets)
}

// homesFor returns the interfaces the sockets to dst were bound to, in
// order.
func (l *bindLog) homesFor(dst netip.Addr) []string {
	var homes []string
	for _, s := range l.all() {
		if s.dst == dst {
			homes = append(homes, s.home)
		}
	}
	return homes
}

func both(fs ...func(*Config)) func(*Config) {
	return func(c *Config) {
		for _, f := range fs {
			f(c)
		}
	}
}

func TestSharingStaysOnWhileAVPNCarriesTheDefaultRoute(t *testing.T) {
	route := newFakeRoute(Route{Home: homeEn0})
	g := newFakeGateway(t)
	a := startAgent(t, g, route.configure)
	c := g.nextConn(t)
	waitState(t, a.Agent, StateConnected)

	// A Use tunnel comes up; the LAN's default route stays.
	route.set(Route{Home: homeEn0, Tunneled: true})
	g.noConnFor(t, quietPeriod)
	if err := c.conn.Context().Err(); err != nil {
		t.Fatalf("the VPN ended the gateway connection: %v", context.Cause(c.conn.Context()))
	}
	if s := a.Stats(); s.State != StateConnected || s.OtherVPN {
		t.Fatalf("Stats() = %+v, want connected", s)
	}
}

func TestSharingPausesWhileOnlyAVPNReachesTheInternet(t *testing.T) {
	route := newFakeRoute(Route{Home: homeEn0})
	g := newFakeGateway(t)
	a := startAgent(t, g, route.configure)
	c := g.nextConn(t)

	// A kill-switch VPN: no default route but the tunnel's.
	route.set(Route{Tunneled: true})
	assertGoingAway(t, c.waitClosed(t))
	waitState(t, a.Agent, StatePaused)
	if !a.Stats().OtherVPN {
		t.Fatal("Stats().OtherVPN = false while only a VPN reaches the internet")
	}
	g.noConnFor(t, quietPeriod)

	// A member's Pause outlasts the VPN.
	a.Pause()
	route.set(Route{Home: homeEn0, Tunneled: true})
	g.noConnFor(t, quietPeriod)

	a.Resume()
	g.nextConn(t)
	waitState(t, a.Agent, StateConnected)
}

func TestAgentDoesNotConnectWhileOnlyAVPNReachesTheInternetAtStart(t *testing.T) {
	g := newFakeGateway(t)
	a := startAgent(t, g, newFakeRoute(Route{Tunneled: true}).configure)
	g.noConnFor(t, quietPeriod)
	if s := a.Stats(); s.State != StatePaused || !s.OtherVPN {
		t.Fatalf("Stats() = %+v, want paused because of another VPN", s)
	}
}

func TestTheAgentReconnectsThroughANewHomeInterface(t *testing.T) {
	route := newFakeRoute(Route{Home: homeEn0})
	binds := &bindLog{}
	g := newFakeGateway(t)
	a := startAgent(t, g, both(route.configure, binds.configure))
	first := g.nextConn(t)

	// The machine moves from Wi-Fi to Ethernet while a VPN is on.
	route.set(Route{Home: homeEn10, Tunneled: true})
	assertGoingAway(t, first.waitClosed(t))
	g.nextConn(t)
	waitState(t, a.Agent, StateConnected)

	gw := netip.MustParseAddrPort(g.ln.Addr().String()).Addr()
	if got, want := binds.homesFor(gw), []string{"en0", "en10"}; !slices.Equal(got, want) {
		t.Fatalf("gateway sockets bound to %v, want %v", got, want)
	}
}

func TestEverySocketTheAgentOpensIsBoundToTheHomeInterface(t *testing.T) {
	tcpOrigin := startTCPOrigin(t, func(c *net.TCPConn) { _, _ = io.Copy(c, c) })
	udpOrigin := startUDPEcho(t)
	doh := newFakeDoH(t, map[string]dohAnswer{"example.org. A": {addrs: []netip.Addr{public4}}})
	binds := &bindLog{}
	g := newFakeGateway(t)
	startAgent(t, g, both(
		newFakeRoute(Route{Home: homeEn0, Tunneled: true}).configure,
		binds.configure,
		allowOrigins(tcpOrigin, udpOrigin),
		func(c *Config) { c.Resolver = newDoHResolver([]dohServer{doh.server()}, doh.roots()) },
	))
	c := g.nextConn(t)
	_ = c.mustDial(t, relayprotocol.KindTCP, tcpOrigin)
	_ = c.mustDial(t, relayprotocol.KindUDP, udpOrigin)
	exchange(t, c.mustDial(t, relayprotocol.KindDNS, virtualResolver), query(t, 1, "example.org.", dnsmessage.TypeA))

	gw := netip.MustParseAddrPort(g.ln.Addr().String()).Addr()
	want := []boundSocket{
		{"udp4", gw, "en0"},
		{"tcp4", tcpOrigin.Addr(), "en0"},
		{"udp4", udpOrigin.Addr(), "en0"},
		{"tcp4", doh.server().Addrs[0].Addr(), "en0"},
	}
	got := binds.all()
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("no socket %+v among the bound sockets %+v", w, got)
		}
	}
}

func TestAgentReportsWhatItRelayed(t *testing.T) {
	origin := startTCPOrigin(t, func(c *net.TCPConn) {
		req, _ := io.ReadAll(c)
		_, _ = c.Write(append(req, req...))
	})
	g := newFakeGateway(t)
	a := startAgent(t, g, allowOrigins(origin))
	c := g.nextConn(t)

	s := c.mustDial(t, relayprotocol.KindTCP, origin)
	_, _ = s.Write([]byte("12345"))
	_ = s.Close()
	readAll(t, s)

	c.waitStats(t, func(st relayprotocol.Stats) bool {
		return st == relayprotocol.Stats{FlowsActive: 0, FlowsTotal: 1, BytesOut: 5, BytesIn: 10}
	})

	// The gateway's counters restart with each control stream; the
	// agent's own count does not.
	_ = c.conn.CloseWithError(relayprotocol.CodeInternalError.ApplicationErrorCode(), "")
	next := g.nextConn(t)
	next.waitStats(t, func(st relayprotocol.Stats) bool { return st == relayprotocol.Stats{} })
	if got := a.Stats(); got.FlowsTotal != 1 || got.BytesOut != 5 || got.BytesIn != 10 || got.FlowsActive != 0 {
		t.Fatalf("Stats() = %+v, want 1 flow, 5 bytes out and 10 in", got)
	}
}

// assertGoingAway checks that the agent closed its connection with
// GOING_AWAY.
func assertGoingAway(t *testing.T, err error) {
	t.Helper()
	var appErr *quic.ApplicationError
	if !errors.As(err, &appErr) || !appErr.Remote || appErr.ErrorCode != relayprotocol.CodeGoingAway.ApplicationErrorCode() {
		t.Fatalf("connection ended with %v, want GOING_AWAY from the agent", err)
	}
}

func TestQuickPauseAndResumeIsNotAConnectionFailure(t *testing.T) {
	g := newFakeGateway(t)
	a := startAgent(t, g, nil)
	first := g.nextConn(t)

	a.Pause()
	a.Resume()
	assertGoingAway(t, first.waitClosed(t))
	g.nextConn(t)
	waitState(t, a.Agent, StateConnected)
	if logs := a.logs.String(); strings.Contains(logs, "reconnecting") {
		t.Fatalf("a pause was logged as a failed connection:\n%s", logs)
	}
}

func TestAgentWaitsLongAfterTheGatewayRejectsIt(t *testing.T) {
	g := newFakeGateway(t)
	startAgent(t, g, func(c *Config) {
		c.ReconnectMin = 10 * time.Millisecond
		c.ReconnectMax = 2 * time.Second // the wait after a rejection: 1 to 2 s
	})
	c := g.nextConn(t)
	_ = c.conn.CloseWithError(relayprotocol.CodeUnauthorized.ApplicationErrorCode(), "")

	g.noConnFor(t, 700*time.Millisecond)
	g.nextConn(t)
}

func TestAgentRetriesSoonWhileTheGatewayRefusesItsCertificate(t *testing.T) {
	g := newFakeGateway(t)
	// A gateway refuses a new line's agent until it has fetched its config
	// again, which the refusal makes it do at once (or within 2 s).
	const refusals = 8
	g.refuseCertificates(refusals)
	startAgent(t, g, func(c *Config) {
		c.ReconnectMin = 200 * time.Millisecond
		c.ReconnectMax = 2 * time.Second
	})
	g.nextConn(t)

	at := g.handshakes()
	if len(at) != refusals+1 {
		t.Fatalf("%d handshakes; want %d refused, then one accepted", len(at), refusals)
	}
	// The first five retries wait 100 to 200 ms each, at most 1 s in all.
	// Doubling from 200 ms would take at least 2.5 s for them.
	if took := at[5].Sub(at[0]); took > 1800*time.Millisecond {
		t.Errorf("five retries after a refused certificate took %v; want them about ReconnectMin apart", took)
	}
	// Then the usual backoff resumes, so an agent the gateway never learns
	// does not keep hammering it: the eighth retry waits 400 to 800 ms.
	if gap := at[8].Sub(at[7]); gap < 300*time.Millisecond {
		t.Errorf("the eighth retry came %v after the refusal; want the usual backoff", gap)
	}
}
