package controlplane_test

import (
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
	"github.com/jermainee/homeport/gateway/internal/controlplane"
)

func TestUsageIsCutEvery30Seconds(t *testing.T) {
	f := newFixture(t)
	f.access(t)

	f.gw.send(t, peerAKey, 100, 200)
	f.step(t, 29*time.Second)
	f.flush(t)
	if n := len(f.api.usageEvents()); n != 0 {
		t.Fatalf("%d events after 29 s, want 0", n)
	}
	f.step(t, time.Second)
	f.flush(t)

	events := f.api.usageEvents()
	if len(events) != 1 {
		t.Fatalf("%d events after 30 s, want 1", len(events))
	}
	e := events[0]
	if e.Seq != 0 || e.BytesUp != 100 || e.BytesDown != 200 || e.Final {
		t.Errorf("event is %+v; want seq 0 with 100 up and 200 down", e)
	}
	if e.PeriodStart != "2026-11-13T09:00:00.000Z" || e.PeriodEnd != "2026-11-13T09:00:30.000Z" {
		t.Errorf("period is %s..%s, want the session's first 30 s", e.PeriodStart, e.PeriodEnd)
	}
	if e.SessionStart == nil || e.SessionStart.StartedAt != e.PeriodStart || e.SessionStart.ExitIP != "203.0.113.47" {
		t.Errorf("session_start is %+v; want the start time and the agent's observed IP", e.SessionStart)
	}
	if e.PeerID != peerAID || e.LineID != lineID || e.LeaseID != leaseID(1) {
		t.Errorf("event names peer %s, line %s, lease %s", e.PeerID, e.LineID, e.LeaseID)
	}
}

func TestUsageIsCutEvery50MB(t *testing.T) {
	f := newFixture(t)
	f.access(t)

	f.gw.send(t, peerAKey, 1, 49_999_998)
	f.step(t, time.Second)
	f.flush(t)
	if n := len(f.api.usageEvents()); n != 0 {
		t.Fatalf("%d events one byte before 50 MB, want 0", n)
	}
	f.gw.send(t, peerAKey, 0, 1)
	f.step(t, time.Second)
	f.gw.send(t, peerAKey, 0, 7)
	f.step(t, 30*time.Second)
	f.flush(t)

	events := f.api.usageEvents()
	if len(events) != 2 || events[0].bytes() != 50_000_000 || events[1].bytes() != 7 {
		t.Fatalf("events %+v; want one at 50 MB, then the rest 30 s later", events)
	}
	if events[1].Seq != 1 || events[1].SessionStart != nil || events[1].PeriodStart != events[0].PeriodEnd {
		t.Errorf("second event %+v; want seq 1, no session_start, starting where the first ended", events[1])
	}
}

func TestQuietSessionSendsNoEmptyEvents(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.gw.send(t, peerAKey, 1, 1)
	f.step(t, 30*time.Second)

	for range 3 {
		f.step(t, 30*time.Second) // quiet, but not idle for 2 minutes yet
	}
	f.flush(t)

	if n := len(f.api.usageEvents()); n != 1 {
		t.Errorf("%d events, want 1: periods without bytes are merged into the next event", n)
	}
}

func TestIdleSessionEndsWithAFinalEventAndReturnsItsLease(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.gw.send(t, peerAKey, 1000, 2000)
	f.step(t, time.Second)

	f.step(t, 119*time.Second)
	f.flush(t)
	if n := len(f.api.returned()); n != 0 {
		t.Fatal("the lease was returned before the session was idle for 2 minutes")
	}
	f.step(t, time.Second)
	f.flush(t)

	events := f.api.usageEvents()
	last := events[len(events)-1]
	if !last.Final {
		t.Fatalf("last event %+v is not final", last)
	}
	if got := sumBytes(events); got != 3000 {
		t.Errorf("events report %d bytes, want 3000", got)
	}
	if got := f.api.returned(); len(got) != 1 || got[0] != leaseID(1) {
		t.Errorf("returned leases %v, want lease 1", got)
	}
}

func TestRetriedEventKeepsItsEventID(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.gw.send(t, peerAKey, 10, 20)
	f.step(t, 30*time.Second)
	f.api.failUsage(1)

	f.flush(t) // 503
	f.flush(t)

	events := f.api.usageEvents()
	if len(events) != 2 {
		t.Fatalf("API received %d events, want the same one twice", len(events))
	}
	if events[0].EventID != events[1].EventID {
		t.Errorf("the retry has event_id %s, the first attempt %s", events[1].EventID, events[0].EventID)
	}
	var bodies []string
	for _, c := range f.api.callLog() {
		if c.op == "usage" {
			bodies = append(bodies, c.body)
		}
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Errorf("the retry's body differs from the first attempt's:\n%s\n%s", bodies[0], bodies[1])
	}
	f.flush(t)
	if n := len(f.api.usageEvents()); n != 2 {
		t.Error("an applied event was sent again")
	}
}

func TestRejectedEventIsNotRetried(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.gw.send(t, peerAKey, 10, 20)
	f.step(t, 30*time.Second)
	f.api.rejectSeqs(0)

	f.flush(t)
	f.flush(t)

	if n := len(f.api.usageEvents()); n != 1 {
		t.Errorf("API received %d events, want 1: a rejected event is not retried", n)
	}
}

func TestLeaseIsReturnedOnlyAfterTheFinalEventIsReported(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.gw.send(t, peerAKey, 10, 20)
	f.step(t, time.Second)
	f.step(t, controlplane.IdleTimeout)
	f.api.failUsage(1)

	f.flush(t)
	if n := len(f.api.returned()); n != 0 {
		t.Fatal("the lease was returned while its final event was unreported")
	}
	f.flush(t)

	if got := f.api.returned(); len(got) != 1 || got[0] != leaseID(1) {
		t.Fatalf("returned leases %v, want lease 1", got)
	}
	calls := f.api.callLog()
	lastUsage, firstReturn := -1, -1
	for i, c := range calls {
		if c.op == "usage" {
			lastUsage = i
		}
		if c.op == "return" && firstReturn < 0 {
			firstReturn = i
		}
	}
	if firstReturn < lastUsage {
		t.Error("the lease return was sent before the final event was reported")
	}
}

func TestUnusedNextLeaseIsReturnedToo(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.gw.send(t, peerAKey, 0, renewAtBytes)
	f.step(t, time.Second) // renews: lease 2 is granted, not used yet

	f.step(t, controlplane.IdleTimeout)
	f.flush(t)

	got := f.api.returned()
	if len(got) != 2 || got[0] != leaseID(1) || got[1] != leaseID(2) {
		t.Errorf("returned leases %v, want lease 1 and its unused successor", got)
	}
}

func TestReportedTotalsEqualTheMeter(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	for i := range 20 {
		f.gw.send(t, peerAKey, uint64(1000+i*7919), uint64(30_000_000+i*104_729))
		f.step(t, 7*time.Second)
	}
	f.step(t, controlplane.IdleTimeout) // session 1 ends
	f.gw.send(t, peerAKey, 123, 456)
	f.step(t, time.Second) // session 2 starts from traffic alone
	f.ctl.Settle()
	f.gw.send(t, peerAKey, 1, 1)
	want := f.gw.peerUsage(t, peerAKey)
	f.step(t, time.Second)
	f.step(t, controlplane.IdleTimeout) // session 2 ends
	f.flush(t)

	var got gateway.Usage
	sessions := map[string]bool{}
	for _, e := range f.api.usageEvents() {
		got.Up += e.BytesUp
		got.Down += e.BytesDown
		sessions[e.SessionID] = true
	}
	if got != want {
		t.Errorf("events report %+v, the meter counted %+v", got, want)
	}
	if len(sessions) != 2 {
		t.Errorf("events belong to %d sessions, want 2", len(sessions))
	}
}

func TestRemovedPeersSessionEndsWithItsFinalUsage(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.gw.send(t, peerAKey, 5, 6)
	want := f.gw.peerUsage(t, peerAKey)
	f.api.setConfig(func(cfg map[string]any) {
		cfg["etag"] = `"cfg-2"`
		cfg["peers"] = cfg["peers"].([]any)[1:] // peer A is gone
	})

	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	f.flush(t)

	events := f.api.usageEvents()
	if len(events) != 1 || !events[0].Final || events[0].BytesUp != want.Up || events[0].BytesDown != want.Down {
		t.Errorf("events %+v; want one final event with %+v", events, want)
	}
	if got := f.api.returned(); len(got) != 1 {
		t.Errorf("returned leases %v, want the session's lease", got)
	}
}

func TestEventsAreBatchedAtMost100(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	for range 150 {
		f.gw.send(t, peerAKey, 0, 1)
		f.step(t, 30*time.Second)
	}
	f.flush(t)

	if n := len(f.api.usageEvents()); n != 150 {
		t.Fatalf("API received %d events, want 150", n)
	}
	usageCalls := 0
	for _, c := range f.api.callLog() {
		if c.op == "usage" {
			usageCalls++
		}
	}
	if usageCalls != 2 {
		t.Errorf("%d usage requests for 150 events, want 2", usageCalls)
	}
}

func TestNoSessionStartsAfterClose(t *testing.T) {
	f := newFixture(t)
	f.ctl.Close(t.Context())

	if got := f.access(t); got != gateway.AccessNone {
		t.Errorf("access is %v after Close, want none", got)
	}
	f.gw.send(t, peerAKey, 1, 1)
	f.step(t, time.Second)
	if n := len(f.api.leaseRequests()); n != 0 {
		t.Errorf("%d lease requests after Close, want 0: their leases would never be returned", n)
	}
}
