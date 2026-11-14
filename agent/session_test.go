package agent

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

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

func TestSharingPausesWhileAnotherVPNCarriesTheDefaultRoute(t *testing.T) {
	var otherVPN atomic.Bool
	g := newFakeGateway(t)
	a := startAgent(t, g, func(c *Config) {
		c.TunnelCheck = func() (bool, error) { return otherVPN.Load(), nil }
		c.TunnelCheckInterval = 5 * time.Millisecond
	})
	c := g.nextConn(t)

	otherVPN.Store(true)
	assertGoingAway(t, c.waitClosed(t))
	waitState(t, a.Agent, StatePaused)
	if !a.Stats().OtherVPN {
		t.Fatal("Stats().OtherVPN = false while another VPN carries the route")
	}
	g.noConnFor(t, quietPeriod)

	// A member's Pause outlasts the VPN.
	a.Pause()
	otherVPN.Store(false)
	g.noConnFor(t, quietPeriod)

	a.Resume()
	g.nextConn(t)
	waitState(t, a.Agent, StateConnected)
}

func TestAgentDoesNotConnectWhileAnotherVPNIsUpAtStart(t *testing.T) {
	g := newFakeGateway(t)
	a := startAgent(t, g, func(c *Config) {
		c.TunnelCheck = func() (bool, error) { return true, nil }
	})
	g.noConnFor(t, quietPeriod)
	if s := a.Stats(); s.State != StatePaused || !s.OtherVPN {
		t.Fatalf("Stats() = %+v, want paused because of another VPN", s)
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
		c.ReconnectMin = 100 * time.Millisecond
		c.ReconnectMax = time.Second
	})
	g.nextConn(t)

	at := g.handshakes()
	if len(at) != refusals+1 {
		t.Fatalf("%d handshakes; want %d refused, then one accepted", len(at), refusals)
	}
	// The first five retries come about ReconnectMin apart. Doubling from
	// 100 ms would take at least 1.25 s for them.
	if took := at[5].Sub(at[0]); took > time.Second {
		t.Errorf("five retries after a refused certificate took %v; want them about ReconnectMin apart", took)
	}
	// Then the usual backoff resumes, so an agent the gateway never learns
	// does not keep hammering it: the eighth retry waits 200 to 400 ms.
	if gap := at[8].Sub(at[7]); gap < 150*time.Millisecond {
		t.Errorf("the eighth retry came %v after the refusal; want the usual backoff", gap)
	}
}
