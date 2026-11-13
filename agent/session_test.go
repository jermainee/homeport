package agent

import (
	"errors"
	"io"
	"net"
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
