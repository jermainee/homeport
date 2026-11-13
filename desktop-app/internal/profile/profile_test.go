package profile_test

import (
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
