package main

import (
	"os"
	"path/filepath"
	"testing"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

func TestStateIsCreatedOnceAndKept(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	first, err := loadState(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := loadState(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if first.wgKey != second.wgKey {
		t.Error("the WireGuard key changed between starts")
	}
	if relayprotocol.PinOf(first.cert.Leaf) != relayprotocol.PinOf(second.cert.Leaf) {
		t.Error("the relay certificate's pin changed between starts; agents would refuse the gateway")
	}
	for _, name := range []string{"wg.key", "relay.key", "relay.crt"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if perm := info.Mode().Perm(); name != "relay.crt" && perm != 0o600 {
			t.Errorf("%s has mode %o, want 600", name, perm)
		}
	}
}

func TestDamagedStateIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wg.key"), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadState(dir); err == nil {
		t.Fatal("a damaged key was replaced instead of reported; a new key would break every peer's config")
	}
}
