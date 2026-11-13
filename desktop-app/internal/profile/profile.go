// Package profile keeps what one Homeport install remembers between runs:
// its install id and the member's session. Each profile is one directory,
// so two dev accounts can run side by side with different HOMEPORT_PROFILE
// values.
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uuid"
)

// Profile is an open profile directory.
type Profile struct {
	dir       string
	installID string
}

// Dir returns the profile directory: HOMEPORT_PROFILE, or Homeport in the
// user's config directory (~/Library/Application Support/Homeport on macOS).
func Dir(getenv func(string) string) (string, error) {
	if d := getenv("HOMEPORT_PROFILE"); d != "" {
		return filepath.Abs(d)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	return filepath.Join(base, "Homeport"), nil
}

// Open opens the profile in dir, creating it and its install id on first use.
func Open(dir string) (*Profile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	p := &Profile{dir: dir}
	id, err := p.loadInstallID()
	if err != nil {
		return nil, err
	}
	p.installID = id
	return p, nil
}

// Dir returns the profile's directory.
func (p *Profile) Dir() string { return p.dir }

// InstallID identifies this install. It never changes for the profile, so
// the API can tell when two accounts sign in from the same install.
func (p *Profile) InstallID() string { return p.installID }

func (p *Profile) loadInstallID() (string, error) {
	path := filepath.Join(p.dir, "install-id")
	b, err := os.ReadFile(path)
	if err == nil {
		if id, err := uuid.Parse(strings.TrimSpace(string(b))); err == nil {
			return id.String(), nil
		}
		return "", fmt.Errorf("profile: %s is not a UUID", path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("profile: %w", err)
	}
	id := uuid.NewV4().String()
	if err := writeFileAtomic(path, []byte(id+"\n")); err != nil {
		return "", err
	}
	return id, nil
}

// writeFileAtomic replaces path with data, readable only by the user, so a
// crash never leaves a half-written file behind.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("profile: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("profile: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("profile: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	return nil
}

// Session is a signed-in member's session.
//
// The token is kept in a file only the user can read. Unsigned dev builds
// can't use the macOS Keychain without a prompt after every rebuild; the
// Keychain comes with signed release builds.
type Session struct {
	Token     string    `json:"token"`
	MemberID  string    `json:"member_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (p *Profile) sessionPath() string { return filepath.Join(p.dir, "session.json") }

// Session returns the saved session, if there is one.
func (p *Profile) Session() (Session, bool, error) {
	b, err := os.ReadFile(p.sessionPath())
	if errors.Is(err, os.ErrNotExist) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("profile: %w", err)
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil || s.Token == "" {
		return Session{}, false, fmt.Errorf("profile: %s is not a session", p.sessionPath())
	}
	return s, true, nil
}

// SaveSession saves s, replacing any earlier session.
func (p *Profile) SaveSession(s Session) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	return writeFileAtomic(p.sessionPath(), b)
}

// ClearSession forgets the session. Clearing when there is none is fine.
func (p *Profile) ClearSession() error {
	if err := os.Remove(p.sessionPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("profile: %w", err)
	}
	return nil
}
