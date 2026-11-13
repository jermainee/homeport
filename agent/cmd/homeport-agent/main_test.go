package main

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

func TestKeyIsCreatedOnceAndKeepsItsPin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.key")

	first, created, err := loadOrCreateKey(path)
	if err != nil || !created {
		t.Fatalf("first load: created %v, error %v; want a new key", created, err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v, want 0600", info.Mode().Perm())
	}
	second, created, err := loadOrCreateKey(path)
	if err != nil || created {
		t.Fatalf("second load: created %v, error %v; want the stored key", created, err)
	}
	if pinOf(t, first) != pinOf(t, second) {
		t.Fatal("the stored key has another pin than the one created")
	}
}

func TestPrintPinPrintsTheAgentPin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.key")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-key", path, "-print-pin"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, stderr.String())
	}
	key, _, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(stdout.String()), pinOf(t, key).String(); got != want {
		t.Fatalf("printed %q, want the key's pin %q", got, want)
	}
}

func TestMissingGatewayIsAUsageError(t *testing.T) {
	t.Setenv("HOMEPORT_GATEWAY", "")
	t.Setenv("HOMEPORT_GATEWAY_PIN", "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-key", filepath.Join(t.TempDir(), "agent.key")}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-gateway") {
		t.Fatalf("exit code %d, stderr %q; want 2 and a hint about -gateway", code, stderr.String())
	}
}

func pinOf(t *testing.T, key ed25519.PrivateKey) relayprotocol.Pin {
	t.Helper()
	pin, err := relayprotocol.PinOfPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	return pin
}
