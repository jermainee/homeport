package profile

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"uuid"
)

// Share is what the profile remembers about one member's Share between runs.
type Share struct {
	// DeviceID and LineID are this computer's device and line at the API,
	// once registered.
	DeviceID string `json:"device_id,omitempty"`
	LineID   string `json:"line_id,omitempty"`
	// On is the member's choice: whether Share should run.
	On bool `json:"on"`
	// APIOn is what the API last confirmed for the line's sharing_enabled.
	APIOn bool `json:"api_on"`
	// Day (YYYY-MM-DD, local time) and DayBytes count the data shared that
	// day, so a restart doesn't reset "shared today".
	Day      string `json:"day,omitempty"`
	DayBytes uint64 `json:"day_bytes,omitempty"`
}

// memberDir is the directory for one member's Share. Member ids come from
// the API, so only a canonical UUID may become a path.
func (p *Profile) memberDir(memberID string) (string, error) {
	id, err := uuid.Parse(memberID)
	if err != nil || id.String() != memberID {
		return "", fmt.Errorf("profile: member id %q is not a lowercase UUID", memberID)
	}
	dir := filepath.Join(p.dir, "members", memberID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	return dir, nil
}

// AgentKey returns the Ed25519 key member's sharing agent identifies with,
// creating it on first use. Each member who signs in on this profile gets
// their own key, because the API ties a key to one member.
//
// The key is a PEM PKCS #8 file only the user can read, like the session;
// the Keychain comes with signed release builds.
func (p *Profile) AgentKey(memberID string) (ed25519.PrivateKey, error) {
	dir, err := p.memberDir(memberID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "agent-key.pem")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return createAgentKey(path)
	}
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	// A damaged key is an error, not a reason for a new one: the API knows
	// this key's device and line, and a new key would leave them behind.
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("profile: %s holds no PEM PRIVATE KEY block", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("profile: %s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("profile: %s holds a %T, want an Ed25519 key", path, parsed)
	}
	return key, nil
}

func createAgentKey(path string) (ed25519.PrivateKey, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	if err := writeFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return nil, err
	}
	return key, nil
}

// Share returns what the profile remembers about member's Share. A missing
// or damaged file is a fresh start: registering the same key again returns
// the same device.
func (p *Profile) Share(memberID string) (Share, error) {
	dir, err := p.memberDir(memberID)
	if err != nil {
		return Share{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "share.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Share{}, nil
	}
	if err != nil {
		return Share{}, fmt.Errorf("profile: %w", err)
	}
	var s Share
	if json.Unmarshal(data, &s) != nil {
		return Share{}, nil
	}
	return s, nil
}

// SaveShare saves what the profile remembers about member's Share.
func (p *Profile) SaveShare(memberID string, s Share) error {
	dir, err := p.memberDir(memberID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	return writeFileAtomic(filepath.Join(dir, "share.json"), data)
}
