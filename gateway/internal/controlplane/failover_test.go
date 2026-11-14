package controlplane_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
)

// The public IPs of peer A's own line (as newFixture brings it online) and
// of its fallback line.
var (
	ownLineIP  = netip.MustParseAddr("203.0.113.47")
	fallbackIP = netip.MustParseAddr("198.51.100.20")
)

func (f *fixture) lineOnline(id string, ip netip.Addr) {
	f.ctl.OnLineEvent(gateway.LineEvent{LineID: id, Online: true, ObservedIP: ip, AgentVersion: "0.1.0"})
}

func (f *fixture) lineOffline(id string, ip netip.Addr) {
	f.ctl.OnLineEvent(gateway.LineEvent{LineID: id, Online: false, ObservedIP: ip, AgentVersion: "0.1.0"})
}

// busyStep sends a few bytes for peer A, so its session does not go idle,
// then steps.
func (f *fixture) busyStep(t *testing.T, d time.Duration) {
	t.Helper()
	f.gw.send(t, peerAKey, 100, 1_000)
	f.step(t, d)
}

// lastLeaseLine is the line_id of the newest lease request.
func (f *fixture) lastLeaseLine(t *testing.T) string {
	t.Helper()
	reqs := f.api.leaseRequests()
	if len(reqs) == 0 {
		t.Fatal("no lease was requested")
	}
	return reqs[len(reqs)-1].LineID
}

// finalEvents are the final usage events the API received.
func (f *fixture) finalEvents(t *testing.T) []usageEvent {
	t.Helper()
	f.flush(t)
	var finals []usageEvent
	for _, e := range f.api.usageEvents() {
		if e.Final {
			finals = append(finals, e)
		}
	}
	return finals
}

func TestASessionStartsOnThePeersOwnLineWhileItIsOnline(t *testing.T) {
	f := newFixture(t)
	f.lineOnline(fallbackLineID, fallbackIP)

	if got := f.access(t); got != gateway.AccessFull {
		t.Fatalf("access %v, want full", got)
	}
	if line := f.lastLeaseLine(t); line != lineID {
		t.Errorf("lease for line %s, want the peer's own line %s", line, lineID)
	}
	if line := f.peerA(t).LineID; line != lineID {
		t.Errorf("the gateway sends peer A's flows to %s, want %s", line, lineID)
	}
}

func TestASessionStartsOnAFallbackLineWhileThePeersLineIsOffline(t *testing.T) {
	f := newFixture(t)
	f.lineOffline(lineID, ownLineIP)
	f.lineOnline(fallbackLineID, fallbackIP)

	if got := f.access(t); got != gateway.AccessFull {
		t.Fatalf("access %v, want full on the fallback line", got)
	}
	f.busyStep(t, 31*time.Second)
	f.flush(t)

	if line := f.lastLeaseLine(t); line != fallbackLineID {
		t.Errorf("lease for line %s, want the fallback line", line)
	}
	if line := f.peerA(t).LineID; line != fallbackLineID {
		t.Errorf("the gateway sends peer A's flows to %s, want the fallback line", line)
	}
	events := f.api.usageEvents()
	if len(events) == 0 || events[0].LineID != fallbackLineID || events[0].SessionStart == nil || events[0].SessionStart.ExitIP != fallbackIP.String() {
		t.Errorf("usage events %+v; want the session on the fallback line, exiting at %s", events, fallbackIP)
	}
}

func TestNoLineOnlineMeansNoAccess(t *testing.T) {
	f := newFixture(t)
	f.lineOffline(lineID, ownLineIP)

	if got := f.access(t); got != gateway.AccessNone {
		t.Errorf("access %v with no line online, want none", got)
	}
	if n := len(f.api.leaseRequests()); n != 0 {
		t.Errorf("%d lease requests, want none", n)
	}
}

func TestASessionMovesToAFallbackLineOnceItsLineHasBeenOfflineForTenSeconds(t *testing.T) {
	f := newFixture(t)
	f.lineOnline(fallbackLineID, fallbackIP)
	f.access(t)
	f.busyStep(t, time.Second)
	resets := f.gw.resetCount(t, peerAKey)

	f.lineOffline(lineID, ownLineIP)
	f.busyStep(t, 9*time.Second)
	held := len(f.finalEvents(t))
	f.busyStep(t, 2*time.Second)
	finals := f.finalEvents(t)

	if held != 0 {
		t.Fatal("the session ended within 10 seconds of its line going offline")
	}
	if len(finals) != 1 || finals[0].LineID != lineID {
		t.Fatalf("final events %+v; want the session on the peer's own line ended", finals)
	}
	if f.gw.resetCount(t, peerAKey) <= resets {
		t.Error("the peer's open flows were not reset when its session moved")
	}
	if f.gw.isSuspended(t, peerAKey) {
		t.Error("the peer was cut off; moving to another line must not suspend it")
	}
	if got := f.access(t); got != gateway.AccessFull || f.lastLeaseLine(t) != fallbackLineID {
		t.Errorf("next flow: access %v on line %s, want full on the fallback line", got, f.lastLeaseLine(t))
	}
}

func TestABriefDropKeepsTheSessionOnItsLine(t *testing.T) {
	f := newFixture(t)
	f.lineOnline(fallbackLineID, fallbackIP)
	f.access(t)
	f.busyStep(t, time.Second)

	f.lineOffline(lineID, ownLineIP)
	f.busyStep(t, 5*time.Second)
	f.lineOnline(lineID, ownLineIP)
	f.busyStep(t, 20*time.Second)

	if finals := f.finalEvents(t); len(finals) != 0 {
		t.Errorf("final events %+v; a 5-second drop must not move the session", finals)
	}
	if n := len(f.api.leaseRequests()); n != 1 {
		t.Errorf("%d lease requests, want the session's one", n)
	}
}

func TestThePeerMovesBackOnceItsLineHasBeenOnlineForTheFailbackTime(t *testing.T) {
	f := newFixture(t)
	f.lineOffline(lineID, ownLineIP)
	f.lineOnline(fallbackLineID, fallbackIP)
	f.access(t)
	f.busyStep(t, time.Second)

	f.lineOnline(lineID, ownLineIP)
	for range 4 {
		f.busyStep(t, time.Minute)
	}
	f.busyStep(t, 59*time.Second)
	early := len(f.finalEvents(t))
	f.busyStep(t, 2*time.Second)
	finals := f.finalEvents(t)

	if early != 0 {
		t.Fatal("the peer moved back before its line had been online for failback_seconds (300)")
	}
	if len(finals) != 1 || finals[0].LineID != fallbackLineID {
		t.Fatalf("final events %+v; want the session on the fallback line ended", finals)
	}
	if got := f.access(t); got != gateway.AccessFull || f.lastLeaseLine(t) != lineID {
		t.Errorf("next flow: access %v on line %s, want full on the peer's own line", got, f.lastLeaseLine(t))
	}
}

func TestALineThatDropsAgainRestartsTheFailbackTime(t *testing.T) {
	f := newFixture(t)
	f.lineOffline(lineID, ownLineIP)
	f.lineOnline(fallbackLineID, fallbackIP)
	f.access(t)
	f.lineOnline(lineID, ownLineIP)
	for range 4 {
		f.busyStep(t, time.Minute)
	}

	f.lineOffline(lineID, ownLineIP)
	f.lineOnline(lineID, ownLineIP)
	f.busyStep(t, 2*time.Minute)

	if finals := f.finalEvents(t); len(finals) != 0 {
		t.Errorf("final events %+v; the line was back for only 2 minutes since it dropped again", finals)
	}
}

func TestAFallbackLineTheConfigDropsEndsItsSession(t *testing.T) {
	f := newFixture(t)
	f.lineOffline(lineID, ownLineIP)
	f.lineOnline(fallbackLineID, fallbackIP)
	f.access(t)
	f.busyStep(t, time.Second)

	f.api.setConfig(func(cfg map[string]any) {
		cfg["etag"] = `"cfg-2"`
		cfg["peers"].([]any)[0].(map[string]any)["fallback_line_ids"] = []any{}
	})
	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	f.busyStep(t, time.Second)

	if finals := f.finalEvents(t); len(finals) != 1 || finals[0].LineID != fallbackLineID {
		t.Errorf("final events %+v; want the session on the dropped fallback line ended", finals)
	}
	if n := f.gw.removedCount(); n != 0 {
		t.Errorf("%d peers removed from the gateway; a change of lines keeps the peer", n)
	}
	if f.gw.isSuspended(t, peerAKey) {
		t.Error("the peer was cut off")
	}
}

func TestChangingAPeersFallbackLinesKeepsItsSession(t *testing.T) {
	f := newFixture(t)
	f.access(t)
	f.busyStep(t, time.Second)

	f.api.setConfig(func(cfg map[string]any) {
		cfg["etag"] = `"cfg-2"`
		cfg["peers"].([]any)[0].(map[string]any)["fallback_line_ids"] = []any{}
	})
	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	f.busyStep(t, time.Second)

	if finals := f.finalEvents(t); len(finals) != 0 {
		t.Errorf("final events %+v; the session's own line is still the peer's", finals)
	}
	if n := f.gw.removedCount(); n != 0 {
		t.Errorf("%d peers removed from the gateway, want none", n)
	}
}

func TestMovingAPeerToAnotherLineEndsItsSessionOnTheOldOne(t *testing.T) {
	f := newFixture(t)
	f.lineOnline(fallbackLineID, fallbackIP)
	f.access(t)
	f.busyStep(t, time.Second)

	f.api.setConfig(func(cfg map[string]any) {
		cfg["etag"] = `"cfg-2"`
		peer := cfg["peers"].([]any)[0].(map[string]any)
		peer["line_id"], peer["fallback_line_ids"] = fallbackLineID, []any{}
	})
	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	f.busyStep(t, time.Second)

	if finals := f.finalEvents(t); len(finals) != 1 || finals[0].LineID != lineID {
		t.Errorf("final events %+v; want the session on the old line ended", finals)
	}
	if n := f.gw.removedCount(); n != 0 {
		t.Errorf("%d peers removed from the gateway; moving keeps the peer and its WireGuard session", n)
	}
	if got := f.access(t); got != gateway.AccessFull || f.lastLeaseLine(t) != fallbackLineID {
		t.Errorf("next flow: access %v on line %s, want full on the new line", got, f.lastLeaseLine(t))
	}
}

func TestAFallbackLineOfThePeersOwnMemberIsNeverUsed(t *testing.T) {
	f := newFixture(t)
	f.api.setConfig(func(cfg map[string]any) {
		cfg["etag"] = `"cfg-2"`
		for _, l := range cfg["lines"].([]any) {
			if line := l.(map[string]any); line["line_id"] == fallbackLineID {
				line["owner_member_id"] = memberID
			}
		}
	})
	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	f.lineOffline(lineID, ownLineIP)
	f.lineOnline(fallbackLineID, fallbackIP)

	if got := f.access(t); got != gateway.AccessNone {
		t.Errorf("access %v; a member never exits through their own line", got)
	}
}
