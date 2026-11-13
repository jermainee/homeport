package controlplane_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
)

const (
	// From contracts/examples/gateway/lease-granted.json.
	grantedBytes = 200_000_000
	renewAtBytes = 160_000_000
)

func TestFirstFlowStartsASessionWithALease(t *testing.T) {
	f := newFixture(t)

	if got := f.access(t); got != gateway.AccessFull {
		t.Fatalf("access is %v, want full once the lease is granted", got)
	}

	reqs := f.api.leaseRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d lease requests, want 1", len(reqs))
	}
	if reqs[0].PeerID != peerAID || reqs[0].SupersedesLeaseID != "" {
		t.Errorf("first lease request is %+v; want peer A and no superseded lease", reqs[0])
	}
	if f.access(t) != gateway.AccessFull || len(f.api.leaseRequests()) != 1 {
		t.Error("a second flow in the same session asked for another lease")
	}
}

func TestLeaseIsRenewedAt80Percent(t *testing.T) {
	f := newFixture(t)
	f.access(t)

	f.gw.send(t, peerAKey, 60_000_000, renewAtBytes-60_000_000-1)
	f.step(t, time.Second)
	if n := len(f.api.leaseRequests()); n != 1 {
		t.Fatalf("%d lease requests one byte before renew_at_bytes, want 1", n)
	}

	f.gw.send(t, peerAKey, 0, 1)
	f.step(t, time.Second)

	reqs := f.api.leaseRequests()
	if len(reqs) != 2 {
		t.Fatalf("%d lease requests at renew_at_bytes, want 2", len(reqs))
	}
	if reqs[1].SupersedesLeaseID != leaseID(1) || reqs[1].SessionID != reqs[0].SessionID {
		t.Errorf("renewal is %+v; want it to supersede %s in session %s", reqs[1], leaseID(1), reqs[0].SessionID)
	}
	if reqs[1].RequestID == reqs[0].RequestID {
		t.Error("the renewal reused the first request's request_id")
	}
	f.step(t, time.Second)
	if n := len(f.api.leaseRequests()); n != 2 {
		t.Errorf("%d lease requests after the next step, want 2: one renewal per lease", n)
	}
}

func TestSessionMovesToTheNextLeaseWhenTheCurrentOneIsUsedUp(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.gw.send(t, peerAKey, 0, renewAtBytes)
	f.step(t, time.Second) // renews

	f.gw.send(t, peerAKey, 0, grantedBytes-renewAtBytes)
	f.step(t, time.Second) // lease 1 is used up: cut, then lease 2
	f.gw.send(t, peerAKey, 1000, 2000)
	f.step(t, 30*time.Second)
	f.flush(t)

	events := f.api.usageEvents()
	perLease := map[string]uint64{}
	for _, e := range events {
		perLease[e.LeaseID] += e.bytes()
	}
	if perLease[leaseID(1)] != grantedBytes || perLease[leaseID(2)] != 3000 || len(perLease) != 2 {
		t.Errorf("bytes per lease are %v; want all %d of lease 1 against it, and the next 3000 against lease 2", perLease, grantedBytes)
	}
	if last := events[len(events)-1]; last.LeaseID != leaseID(2) {
		t.Errorf("the last event counts against %s, want lease 2", last.LeaseID)
	}
	if f.access(t) != gateway.AccessFull {
		t.Error("access is not full on the next lease")
	}
}

func TestExpiredLeaseIsReplacedByTheNextOne(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.gw.send(t, peerAKey, 0, renewAtBytes)
	f.step(t, time.Second)

	f.gw.send(t, peerAKey, 0, 10)
	f.step(t, time.Hour) // lease 1 expires at 10:00, the clock is past it
	f.flush(t)

	events := f.api.usageEvents()
	last := events[len(events)-1]
	if last.LeaseID != leaseID(1) || last.PeriodEnd != "2026-11-13T10:00:01.000Z" {
		t.Fatalf("last event %+v; want a cut for lease 1 at the step after its expiry", last)
	}
	if got := sumBytes(events); got != renewAtBytes+10 {
		t.Errorf("events report %d bytes against lease 1, want %d", got, renewAtBytes+10)
	}
	if f.access(t) != gateway.AccessFull {
		t.Error("access is not full on the next lease")
	}
}

func TestNoCreditsMeansTopUpOnlyThenTheSessionCloses(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.api.setLeaseFn(func(leaseRequest) (int, any) {
		return http.StatusPaymentRequired, example(t, "lease-denied.json") // grace_seconds 300
	})
	f.gw.send(t, peerAKey, 0, renewAtBytes)
	f.step(t, time.Second) // renewal: 402
	if f.access(t) != gateway.AccessFull {
		t.Fatal("access ended before the current lease was used up")
	}
	resetsBefore := f.gw.resetCount(t, peerAKey)

	f.gw.send(t, peerAKey, 0, grantedBytes-renewAtBytes)
	f.step(t, time.Second)

	if got := f.access(t); got != gateway.AccessTopUp {
		t.Fatalf("access is %v once the lease is used up after a 402, want top-up only", got)
	}
	if f.gw.resetCount(t, peerAKey) == resetsBefore {
		t.Error("open flows were not reset when the session went top-up only")
	}

	f.gw.send(t, peerAKey, 0, 5000) // the top-up page
	f.step(t, 298*time.Second)
	if f.gw.isSuspended(t, peerAKey) {
		t.Fatal("the session closed before grace_seconds")
	}
	f.step(t, 2*time.Second)

	if !f.gw.isSuspended(t, peerAKey) {
		t.Fatal("the session is still open after grace_seconds")
	}
	if got := f.access(t); got != gateway.AccessNone {
		t.Errorf("access is %v after the session closed, want none", got)
	}
	f.flush(t)
	events := f.api.usageEvents()
	last := events[len(events)-1]
	if !last.Final || last.LeaseID != leaseID(1) {
		t.Errorf("last event is %+v; want a final event for lease 1", last)
	}
	if total := sumBytes(events); total != grantedBytes+5000 {
		t.Errorf("events report %d bytes, want %d", total, grantedBytes+5000)
	}
	if got := f.api.returned(); len(got) != 1 || got[0] != leaseID(1) {
		t.Errorf("returned leases %v, want lease 1", got)
	}
}

func TestPeerInactiveClosesTheSessionAtOnce(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.api.setLeaseFn(func(leaseRequest) (int, any) {
		return http.StatusForbidden, example(t, "lease-peer-inactive.json")
	})
	f.gw.send(t, peerAKey, 7, renewAtBytes)

	f.step(t, time.Second) // renewal: 403

	if !f.gw.isSuspended(t, peerAKey) {
		t.Fatal("the session is still open after a 403")
	}
	if got := f.access(t); got != gateway.AccessNone {
		t.Errorf("access is %v after a 403, want none", got)
	}
	f.flush(t)
	events := f.api.usageEvents()
	if len(events) == 0 || !events[len(events)-1].Final {
		t.Fatalf("events %+v; want them to end with a final event", events)
	}
	if total := sumBytes(events); total != renewAtBytes+7 {
		t.Errorf("events report %d bytes, want %d", total, renewAtBytes+7)
	}
}

func TestPeerInactiveAtSessionStartRefusesTheFlow(t *testing.T) {
	f := newFixture(t)
	f.api.setLeaseFn(func(leaseRequest) (int, any) {
		return http.StatusForbidden, example(t, "lease-peer-inactive.json")
	})

	if got := f.access(t); got != gateway.AccessNone {
		t.Fatalf("access is %v, want none", got)
	}
	if !f.gw.isSuspended(t, peerAKey) {
		t.Error("the peer was not cut off")
	}
}

func TestClosedSessionCanStartAgainAfterACooldown(t *testing.T) {
	f := newFixture(t)
	f.api.setLeaseFn(func(leaseRequest) (int, any) {
		return http.StatusForbidden, example(t, "lease-peer-inactive.json")
	})
	f.access(t)
	f.api.setLeaseFn(f.api.grant)

	f.step(t, time.Minute)

	if f.gw.isSuspended(t, peerAKey) {
		t.Fatal("the peer is still cut off a minute after its session closed")
	}
	if got := f.access(t); got != gateway.AccessFull {
		t.Errorf("access is %v in a new session, want full", got)
	}
}

func TestUnreachableAPIMeansTopUpOnlyUntilALeaseIsGranted(t *testing.T) {
	f := newFixture(t)
	f.api.setLeaseFn(func(leaseRequest) (int, any) {
		return http.StatusServiceUnavailable, map[string]string{"error": "unavailable"}
	})

	if got := f.access(t); got != gateway.AccessTopUp {
		t.Fatalf("access is %v while the API fails, want top-up only", got)
	}
	f.api.setLeaseFn(f.api.grant)
	f.step(t, 10*time.Second)

	if got := f.access(t); got != gateway.AccessFull {
		t.Errorf("access is %v once the API grants a lease, want full", got)
	}
	reqs := f.api.leaseRequests()
	if len(reqs) != 2 || reqs[0].RequestID != reqs[1].RequestID {
		t.Errorf("lease requests %+v; a retry after a 5xx must reuse its request_id", reqs)
	}
}

func TestGraceWithoutALeaseClosesTheSession(t *testing.T) {
	f := newFixture(t)
	f.api.setLeaseFn(func(leaseRequest) (int, any) {
		return http.StatusPaymentRequired, map[string]any{"error": "insufficient_credits", "grace_seconds": 60}
	})
	if got := f.access(t); got != gateway.AccessTopUp {
		t.Fatalf("access is %v after a 402, want top-up only", got)
	}

	for range 6 {
		f.step(t, 10*time.Second) // retries every 10 s, in case the member tops up
	}

	if !f.gw.isSuspended(t, peerAKey) {
		t.Fatal("the session is still open after grace_seconds")
	}
	reqs := f.api.leaseRequests()
	for i := 1; i < len(reqs); i++ {
		if reqs[i].RequestID == reqs[i-1].RequestID {
			t.Error("a new lease request after a 402 reused the request_id, which replays the 402")
		}
	}
}

func TestNoSessionStartsWhileTheLineIsOffline(t *testing.T) {
	f := newFixture(t)
	f.ctl.OnLineEvent(gateway.LineEvent{LineID: lineID, Online: false})

	if got := f.access(t); got != gateway.AccessNone {
		t.Fatalf("access is %v while the line is offline, want none", got)
	}
	if n := len(f.api.leaseRequests()); n != 0 {
		t.Errorf("%d lease requests for an offline line, want 0", n)
	}
}

func sumBytes(events []usageEvent) uint64 {
	var n uint64
	for _, e := range events {
		n += e.bytes()
	}
	return n
}
