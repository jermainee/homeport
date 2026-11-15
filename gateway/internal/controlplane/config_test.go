package controlplane_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
)

func TestConfigSyncAppliesTheContractsExample(t *testing.T) {
	f := newBareFixture(t)

	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("sync config: %v", err)
	}

	got, ok := f.gw.peer(t, peerAKey)
	want := gateway.Peer{
		ID:        peerAID,
		PublicKey: mustKey(t, peerAKey),
		IPv4:      netip.MustParseAddr("10.77.0.2"),
		IPv6:      netip.MustParseAddr("fd77:6870::2"),
		MemberID:  memberID,
		LineID:    lineID,
		HTTPSOnly: true,
	}
	if !ok || got != want {
		t.Errorf("peer A on the gateway is %+v, want %+v", got, want)
	}
	if b, ok := f.gw.peer(t, peerBKey); !ok || b.HTTPSOnly {
		t.Errorf("peer B on the gateway is %+v (present %v); want it, without HTTPS only", b, ok)
	}
	line, ok := f.gw.line(lineID)
	wantLine := gateway.Line{ID: lineID, OwnerMemberID: ownerID, AgentPin: mustPin(t, linePin)}
	if !ok || line != wantLine {
		t.Errorf("line on the gateway is %+v, want %+v (the contract's hex SPKI hash as a pin)", line, wantLine)
	}
}

func TestUnchangedConfigIsNotAppliedAgain(t *testing.T) {
	f := newBareFixture(t)
	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	f.gw.RemoveLine(lineID) // would be restored if the config were applied again

	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	tags := f.api.ifNoneMatch()
	if len(tags) != 2 || tags[0] != "" || tags[1] != `"cfg-7f3a9c2e"` {
		t.Fatalf("If-None-Match headers were %q; want none, then the body's etag", tags)
	}
	if _, ok := f.gw.line(lineID); ok {
		t.Fatal("a 304 answer applied the config again")
	}
}

func TestConfigChangesAreApplied(t *testing.T) {
	f := newBareFixture(t)
	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	newPin := "1111111111111111111111111111111111111111111111111111111111111111"
	f.api.setConfig(func(cfg map[string]any) {
		cfg["etag"] = `"cfg-2"`
		cfg["peers"] = cfg["peers"].([]any)[:1] // peer B is gone
		cfg["lines"].([]any)[0].(map[string]any)["device_spki_sha256"] = newPin
	})

	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if _, ok := f.gw.peer(t, peerBKey); ok {
		t.Error("peer B is still on the gateway after the config dropped it")
	}
	if _, ok := f.gw.peer(t, peerAKey); !ok {
		t.Error("peer A was dropped")
	}
	if l, _ := f.gw.line(lineID); l.AgentPin != mustPin(t, newPin) {
		t.Errorf("line pin is %s, want the new one", l.AgentPin)
	}
}

func TestDroppedLineIsRemoved(t *testing.T) {
	f := newBareFixture(t)
	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	f.api.setConfig(func(cfg map[string]any) {
		cfg["etag"] = `"cfg-2"`
		cfg["lines"] = []any{}
	})

	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if _, ok := f.gw.line(lineID); ok {
		t.Error("the line is still on the gateway after the config dropped it")
	}
}

func TestLinesCanSwapAgentPins(t *testing.T) {
	f := newBareFixture(t)
	secondLine := "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e7"
	otherPin := "2222222222222222222222222222222222222222222222222222222222222222"
	f.api.setConfig(func(cfg map[string]any) {
		cfg["lines"] = append(cfg["lines"].([]any), map[string]any{
			"line_id": secondLine, "owner_member_id": ownerID, "device_spki_sha256": otherPin,
		})
	})
	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	f.api.setConfig(func(cfg map[string]any) {
		cfg["etag"] = `"cfg-2"`
		for _, l := range cfg["lines"].([]any) {
			switch line := l.(map[string]any); line["line_id"] {
			case lineID:
				line["device_spki_sha256"] = otherPin
			case secondLine:
				line["device_spki_sha256"] = linePin
			}
		}
	})

	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("sync with swapped pins: %v", err)
	}

	if l, _ := f.gw.line(lineID); l.AgentPin != mustPin(t, otherPin) {
		t.Errorf("first line has pin %s, want the second line's old pin", l.AgentPin)
	}
	if l, _ := f.gw.line(secondLine); l.AgentPin != mustPin(t, linePin) {
		t.Errorf("second line has pin %s, want the first line's old pin", l.AgentPin)
	}
}

func TestARefreshAfterAFailedApplyFetchesOnlyAChangedConfig(t *testing.T) {
	f := newBareFixture(t)
	// A config the gateway cannot apply in full: one line's pin is not hex.
	// Each poll fetches and applies the whole config again until it can.
	f.api.setConfig(func(cfg map[string]any) {
		cfg["lines"] = append(cfg["lines"].([]any), map[string]any{
			"line_id": "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e7", "owner_member_id": ownerID, "device_spki_sha256": "not-hex",
		})
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.ctl.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitFor(t, func() bool { return len(f.api.ifNoneMatch()) == 1 })

	// Unknown agents connect. Refreshes are for new lines; an unchanged
	// config must cost them a 304, not the whole body.
	f.ctl.RefreshConfig()
	waitFor(t, func() bool { return len(f.api.ifNoneMatch()) == 2 })
	if tag := f.api.ifNoneMatch()[1]; tag != `"cfg-7f3a9c2e"` {
		t.Errorf("a refresh after a failed apply sent If-None-Match %q; want the fetched config's etag", tag)
	}
}

// waitFor polls until cond holds, for at most 5 s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTurningHTTPSOnlyOnOrOffAppliesToThePeer(t *testing.T) {
	f := newBareFixture(t)
	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	f.api.setConfig(func(cfg map[string]any) {
		cfg["etag"] = `"cfg-2"`
		cfg["peers"].([]any)[0].(map[string]any)["https_only"] = false
		cfg["peers"].([]any)[1].(map[string]any)["https_only"] = true
	})

	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if a, _ := f.gw.peer(t, peerAKey); a.HTTPSOnly {
		t.Error("peer A still has HTTPS only after the config turned it off")
	}
	// The setting changes in place: the devices keep their WireGuard
	// sessions, and their flows are reset.
	if n := f.gw.removedCount(); n != 0 {
		t.Errorf("%d peers were removed to change HTTPS only, want none", n)
	}
	if f.gw.resetCount(t, peerAKey) == 0 || f.gw.resetCount(t, peerBKey) == 0 {
		t.Error("the peers' flows were not reset when HTTPS only changed")
	}
	if b, _ := f.gw.peer(t, peerBKey); !b.HTTPSOnly {
		t.Error("peer B has no HTTPS only after the config turned it on")
	}
}
