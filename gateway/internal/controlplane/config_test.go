package controlplane_test

import (
	"net/netip"
	"testing"

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
	}
	if !ok || got != want {
		t.Errorf("peer A on the gateway is %+v, want %+v", got, want)
	}
	if _, ok := f.gw.peer(t, peerBKey); !ok {
		t.Error("peer B is not on the gateway")
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
