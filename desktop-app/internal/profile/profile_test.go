package profile_test

import (
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/jermainee/homeport/desktop-app/internal/profile"
)

func TestInstallIDIsStablePerProfile(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()

	first := open(t, dirA).InstallID()
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("install id %q is not a UUID: %v", first, err)
	}
	if again := open(t, dirA).InstallID(); again != first {
		t.Errorf("reopened install id = %q, want %q", again, first)
	}
	if other := open(t, dirB).InstallID(); other == first {
		t.Errorf("two profiles share install id %q", first)
	}
}

func TestDirComesFromHomeportProfile(t *testing.T) {
	want := t.TempDir()
	got, err := profile.Dir(func(k string) string {
		if k == "HOMEPORT_PROFILE" {
			return want
		}
		return ""
	})
	if err != nil || got != want {
		t.Errorf("Dir = %q, %v; want %q", got, err, want)
	}
}

func open(t *testing.T, dir string) *profile.Profile {
	t.Helper()
	p, err := profile.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return p
}

func TestUnreadableInstallIDIsReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "install-id"), []byte("not a uuid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	id := open(t, dir).InstallID()

	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("install id %q is not a UUID: %v", id, err)
	}
	if again := open(t, dir).InstallID(); again != id {
		t.Errorf("reopened install id = %q, want %q", again, id)
	}
}

func TestAgentKeyIsKeptPerMember(t *testing.T) {
	dir := t.TempDir()
	a, b := uuid.NewV7().String(), uuid.NewV7().String()

	keyA, err := open(t, dir).AgentKey(a)
	if err != nil {
		t.Fatalf("AgentKey: %v", err)
	}
	again, err := open(t, dir).AgentKey(a)
	if err != nil || !keyA.Equal(again) {
		t.Errorf("reopened key differs (err %v)", err)
	}
	keyB, err := open(t, dir).AgentKey(b)
	if err != nil || keyA.Equal(keyB) {
		t.Errorf("two members share a key (err %v)", err)
	}
	info, err := os.Stat(filepath.Join(dir, "members", a, "agent-key.pem"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

func TestMemberIDsThatAreNotUUIDsNeverBecomePaths(t *testing.T) {
	p := open(t, t.TempDir())
	for _, id := range []string{"", "..", "../../evil", "01A1D66C-C114-7AE5-A7DF-12CFE002795D"} {
		if _, err := p.AgentKey(id); err == nil {
			t.Errorf("AgentKey(%q) succeeded", id)
		}
		if err := p.SaveShare(id, profile.Share{}); err == nil {
			t.Errorf("SaveShare(%q) succeeded", id)
		}
	}
}
