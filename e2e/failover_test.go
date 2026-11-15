//go:build e2e

package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jermainee/homeport/e2e/internal/fakeapi"
)

// offlineHold is the gateway's controlplane.OfflineHold: how long a session
// stays on a line that went offline before the gateway moves it.
const offlineHold = 10 * time.Second

func TestAPeerMovesToAnotherLineWhileItsLineIsOfflineAndBack(t *testing.T) {
	s := newStack(t, withFallbackLine())
	fallback := s.startFallbackAgent()
	t.Cleanup(func() {
		if t.Failed() {
			s.logSessions()
		}
	})

	// B's peer exits through A's line while it is online.
	if got := s.get("/blob/100000"); sum(got) != sum(blob(100_000)) {
		t.Fatal("download through A's line differs")
	}
	if s.agent.Stats().FlowsTotal == 0 || fallback.Stats().FlowsTotal != 0 {
		t.Fatalf("A's agent saw %d flows and C's %d; want all on A's line", s.agent.Stats().FlowsTotal, fallback.Stats().FlowsTotal)
	}

	// A's line goes offline. Once the gateway's hold is over, B's next
	// flows exit through C's line in the same city. B's WireGuard config
	// stays the same.
	s.stopAgent()
	waitUntil(t, "the gateway saw A's line go offline", func() bool { return !s.gw.Server().LineOnline(lineID) })
	offline := time.Now()
	body := s.getEventually("/blob/200000", offlineHold+waitFor)
	t.Logf("traffic resumed through C's line %v after A's line went offline", time.Since(offline).Round(time.Millisecond))
	if sum(body) != sum(blob(200_000)) {
		t.Fatal("download through C's line differs")
	}
	if fallback.Stats().FlowsTotal == 0 {
		t.Fatal("the download did not go through C's agent")
	}

	// A's line comes back. Once it has been online for failback_seconds,
	// B's peer moves back to it.
	// The gateway's failback time starts when it sees A's line online,
	// which is after this.
	back := time.Now()
	s.startAgent(quickReconnect)
	s.waitLineOnline()
	deadline := back.Add(failbackSeconds*time.Second + waitFor)
	for s.agent.Stats().FlowsTotal == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("B's flows still go through C's line %v after A's line came back", time.Since(back))
		}
		_, _ = s.tryGet("http://"+tcpDst.String()+contentPath+"/blob/1000", 2*time.Second)
		time.Sleep(200 * time.Millisecond)
	}
	if took := time.Since(back); took < failbackSeconds*time.Second {
		t.Errorf("B's peer moved back %v after A's line came back; want after failback_seconds (%d s)", took, failbackSeconds)
	}

	c := s.settled(false)
	s.stopGateway()

	// One session per stay on a line, each reported against that line and
	// so paid to its owner: A's, then C's, then A's again.
	if got := sessionLines(s.api.Usage()); !slices.Equal(got, []string{lineID, fallbackLineID, lineID}) {
		t.Errorf("sessions ran on lines %v, want A's, C's, then A's", got)
	}
	if got := leaseLines(s.api.LeaseRequests()); !slices.Equal(got, []string{lineID, fallbackLineID, lineID}) {
		t.Errorf("leases were asked for lines %v, want A's, C's, then A's", got)
	}
	if owner := lineOwner(s.config, fallbackLineID); owner != fallbackOwner {
		t.Errorf("C's line belongs to %s in the config, want C", owner)
	}
	if bytes := lineBytes(s.api.Usage(), fallbackLineID); bytes < 200_000 {
		t.Errorf("%d bytes reported on C's line, want at least the 200 kB download", bytes)
	}
	if up, down := s.api.UsageTotal(); up != c.gateway.Up || down != c.gateway.Down {
		t.Errorf("usage events report %d up and %d down; the gateway metered %+v", up, down, c.gateway)
	}
}

// logSessions logs the gateway's session, lease and line lines, to tell
// why a failover test failed. They never name a destination.
func (s *stack) logSessions() {
	for line := range strings.SplitSeq(s.gwLogs.String(), "\n") {
		if strings.Contains(line, `"session `) || strings.Contains(line, "lease") || strings.Contains(line, "line") {
			s.t.Log(line)
		}
	}
}

// getEventually fetches path through the tunnel, retrying while flows
// fail, as they do while the peer's line is offline, and starting no new
// attempt after timeout. Flows to an offline line are reset at once, so an
// attempt that connects is let finish within transferTimeout: the race
// detector slows the tunnel down to a few hundred kB/s, less on a slow
// runner.
func (s *stack) getEventually(path string, timeout time.Duration) []byte {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		body, err := s.tryGet("http://"+tcpDst.String()+contentPath+path, transferTimeout)
		if err == nil {
			return body
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("GET %s still fails after %v: %v", path, timeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// sessionLines is the line of each session, in the order the sessions
// started.
func sessionLines(events []fakeapi.UsageEvent) []string {
	var lines []string
	for _, e := range events {
		if e.Seq == 0 {
			lines = append(lines, e.LineID)
		}
	}
	return lines
}

// leaseLines is the line of each session's first lease request, in order.
func leaseLines(reqs []fakeapi.LeaseRequest) []string {
	var lines []string
	for _, r := range reqs {
		if r.SupersedesLeaseID == "" {
			lines = append(lines, r.LineID)
		}
	}
	return lines
}

func lineBytes(events []fakeapi.UsageEvent, lineID string) int64 {
	var n int64
	for _, e := range events {
		if e.LineID == lineID {
			n += e.BytesUp + e.BytesDown
		}
	}
	return n
}

func lineOwner(cfg fakeapi.Config, lineID string) string {
	for _, l := range cfg.Lines {
		if l.LineID == lineID {
			return l.OwnerMemberID
		}
	}
	return ""
}
