package controlplane_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
)

func TestLineOnlineAndOfflineEventsAreReported(t *testing.T) {
	f := newBareFixture(t)
	ip := netip.MustParseAddr("2001:db8:4d2::1c")

	f.ctl.OnLineEvent(gateway.LineEvent{LineID: lineID, Online: true, ObservedIP: ip, AgentVersion: "0.1.0"})
	f.clock.Advance(4 * time.Second)
	f.ctl.OnLineEvent(gateway.LineEvent{LineID: lineID, Online: false, ObservedIP: ip, AgentVersion: "0.1.0"})
	f.flush(t)

	got := f.api.lineEventsReceived()
	if len(got) != 2 {
		t.Fatalf("API received %d line events, want 2", len(got))
	}
	want := []lineEvent{
		{LineID: lineID, Type: "online", ObservedIP: ip.String(), AgentVersion: "0.1.0", At: "2026-11-13T09:00:00.000Z"},
		{LineID: lineID, Type: "offline", ObservedIP: ip.String(), AgentVersion: "0.1.0", At: "2026-11-13T09:00:04.000Z"},
	}
	for i := range want {
		want[i].EventID = got[i].EventID
		if got[i] != want[i] {
			t.Errorf("event %d is %+v, want %+v", i, got[i], want[i])
		}
	}
	if got[0].EventID == got[1].EventID {
		t.Error("both events have the same event_id")
	}
	f.flush(t)
	if n := len(f.api.lineEventsReceived()); n != 2 {
		t.Error("applied line events were sent again")
	}
}

func TestLineEventWithANonSemverAgentVersionIsStillValid(t *testing.T) {
	f := newBareFixture(t)

	f.ctl.OnLineEvent(gateway.LineEvent{LineID: lineID, Online: true, ObservedIP: netip.MustParseAddr("203.0.113.47"), AgentVersion: "dev build"})
	f.flush(t)

	got := f.api.lineEventsReceived()
	if len(got) != 1 || got[0].AgentVersion != "0.0.0" {
		t.Errorf("line events %+v; want one, with version 0.0.0 for an agent version the contract would refuse", got)
	}
}
