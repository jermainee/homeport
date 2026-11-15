package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// The contract's example line (contracts/examples/member/line-created.json).
const (
	exampleLineID = "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e6"
	examplePinHex = "2ca04a183e495439c9a77a4ce5f11babba2758321eb6db8d31e85b3980aea90a"
	memberID      = "01a1c4af-8795-7a28-9249-adbd2ba1c04a"
	sessionToken  = "hpm_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// stubAPI answers the member endpoints the tool calls with the contract's
// examples, and records what it was sent.
type stubAPI struct {
	mu       sync.Mutex
	requests []string // "METHOD /path"
	bodies   map[string]map[string]any
	queries  map[string]string
	auth     map[string]string
}

func newStubAPI(t *testing.T) (*stubAPI, string) {
	t.Helper()
	s := &stubAPI{bodies: map[string]map[string]any{}, queries: map[string]string{}, auth: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.requests = append(s.requests, key)
		s.bodies[key] = body
		s.queries[key] = r.URL.RawQuery
		s.auth[key] = r.Header.Get("Authorization")
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch key {
		case "POST /v1/auth/login-requests":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"login_request_id":"01a1d422-a250-7852-a64a-d4078fcb54e1","match_code":"123456","expires_at":"2099-01-01T00:00:00Z"}`))
		case "POST /dev/login-requests/01a1d422-a250-7852-a64a-d4078fcb54e1/confirm":
			_, _ = w.Write([]byte(`{"status":"approved"}`))
		case "GET /v1/auth/login-requests/01a1d422-a250-7852-a64a-d4078fcb54e1":
			_, _ = w.Write([]byte(`{"status":"approved","member_id":"` + memberID + `","expires_at":"2099-02-01T00:00:00Z"}`))
		case "POST /v1/devices":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"device_id":"01a1c4af-8a50-7c3e-8a0e-1c6f0d3a5b21","spki_sha256":"` + examplePinHex + `"}`))
		case "POST /v1/lines":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"line_id":"` + exampleLineID + `","gateway":{"quic_endpoint":"fra1.gw.homeport.example:443","server_name":"fra1.gw.homeport.example","spki_sha256":"` + examplePinHex + `"}}`))
		case "PATCH /v1/lines/" + exampleLineID:
			_, _ = w.Write([]byte(`{"line_id":"` + exampleLineID + `","sharing_enabled":true}`))
		case "GET /v1/lines/regions":
			_, _ = w.Write(memberExample(t, "line-regions-country.json"))
		case "GET /v1/lines/available":
			_, _ = w.Write(memberExample(t, "lines-available.json"))
		case "POST /v1/wireguard/peers":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(memberExample(t, "wireguard-peer-created.json"))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

// memberExample reads a JSON example from contracts/examples/member.
func memberExample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "examples", "member", name)) // #nosec G304 -- the contract's examples
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (s *stubAPI) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func writeSession(t *testing.T, path string) {
	t.Helper()
	b, _ := json.Marshal(session{Token: sessionToken, MemberID: memberID})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeAgentKey writes a key file as homeport-agent -print-pin -key does.
func writeAgentKey(t *testing.T, path string) ed25519.PublicKey {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestShareRegistersTheAgentKeyAndPrintsTheAgentsSettings(t *testing.T) {
	api, url := newStubAPI(t)
	dir := t.TempDir()
	writeSession(t, filepath.Join(dir, "a.json"))
	pub := writeAgentKey(t, filepath.Join(dir, "agent.key"))
	var stdout, stderr bytes.Buffer

	code := run([]string{"-api", url, "-session", filepath.Join(dir, "a.json"), "share", filepath.Join(dir, "agent.key")}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := []string{"POST /v1/devices", "POST /v1/lines", "PATCH /v1/lines/" + exampleLineID}
	if got := api.sent(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("requests %v, want %v", got, want)
	}
	if got := api.bodies["POST /v1/devices"]["agent_public_key"]; got != base64.StdEncoding.EncodeToString(pub) {
		t.Errorf("registered key %v, want the key file's public key", got)
	}
	if got := api.auth["POST /v1/lines"]; got != "Bearer "+sessionToken {
		t.Errorf("Authorization %q", got)
	}
	if got := api.bodies["PATCH /v1/lines/"+exampleLineID]["sharing_enabled"]; got != true {
		t.Errorf("sharing_enabled %v", got)
	}
	// The agent CLI reads the pin in relay-protocol's text form, not the API's hex.
	pin, err := relayprotocol.ParsePin(envValue(t, stdout.String(), "HOMEPORT_GATEWAY_PIN"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(pin[:]); got != examplePinHex {
		t.Errorf("pin %x, want %s", pin[:], examplePinHex)
	}
	if got := envValue(t, stdout.String(), "HOMEPORT_GATEWAY"); got != "fra1.gw.homeport.example:443" {
		t.Errorf("HOMEPORT_GATEWAY=%s", got)
	}
}

func TestSigninWritesASessionOnlyTheUserCanRead(t *testing.T) {
	_, url := newStubAPI(t)
	path := filepath.Join(t.TempDir(), "sessions", "b.json")
	var stderr bytes.Buffer

	code := run([]string{"-api", url, "-session", path, "signin", "b@example.com", "-dev"}, &bytes.Buffer{}, &stderr)

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	s, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Token, "hpm_") || s.MemberID != memberID || s.ExpiresAt.Year() != 2099 {
		t.Errorf("session %+v", s)
	}
}

func TestSigninNeverReplacesASession(t *testing.T) {
	api, url := newStubAPI(t)
	path := filepath.Join(t.TempDir(), "session.json") // say, a desktop-app profile's
	writeSession(t, path)
	before, _ := os.ReadFile(path) // #nosec G304 -- the test's temp dir
	var stderr bytes.Buffer

	code := run([]string{"-api", url, "-session", path, "signin", "b@example.com", "-dev"}, &bytes.Buffer{}, &stderr)

	if code == 0 {
		t.Fatal("signin replaced an existing session")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) { // #nosec G304 -- the test's temp dir
		t.Error("the session file changed")
	}
	if got := api.sent(); len(got) != 0 {
		t.Errorf("sent %v before refusing", got)
	}
}

func envValue(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, name+"="); ok {
			return v
		}
	}
	t.Fatalf("no %s in:\n%s", name, out)
	return ""
}

func TestPeerInARegionLetsTheAPIPickTheLine(t *testing.T) {
	api, url := newStubAPI(t)
	dir := t.TempDir()
	writeSession(t, filepath.Join(dir, "a.json"))
	var stdout, stderr bytes.Buffer

	code := run([]string{"-api", url, "-session", filepath.Join(dir, "a.json"), "peer", "DE", "Berlin"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	body := api.bodies["POST /v1/wireguard/peers"]
	if body["country"] != "DE" || body["city"] != "Berlin" {
		t.Errorf("body %v, want country DE and city Berlin", body)
	}
	if _, ok := body["line_id"]; ok {
		t.Errorf("body %v names a line_id", body)
	}
	if strings.Contains(stdout.String(), "{{PRIVATE_KEY}}") || !strings.Contains(stdout.String(), "PrivateKey = ") {
		t.Errorf("printed config has no private key:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Berlin, DE") {
		t.Errorf("stderr %q does not say where the line is", stderr.String())
	}
}

func TestPeerOnALineSendsItsID(t *testing.T) {
	api, url := newStubAPI(t)
	dir := t.TempDir()
	writeSession(t, filepath.Join(dir, "a.json"))
	var stdout, stderr bytes.Buffer

	code := run([]string{"-api", url, "-session", filepath.Join(dir, "a.json"), "peer", exampleLineID}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	body := api.bodies["POST /v1/wireguard/peers"]
	if body["line_id"] != exampleLineID {
		t.Errorf("body %v, want line_id %s", body, exampleLineID)
	}
	if _, ok := body["country"]; ok {
		t.Errorf("body %v names a country", body)
	}
}

func TestRegionsAndLinesAskForTheRegion(t *testing.T) {
	api, url := newStubAPI(t)
	dir := t.TempDir()
	writeSession(t, filepath.Join(dir, "a.json"))
	session := filepath.Join(dir, "a.json")

	for _, args := range [][]string{{"regions", "DE"}, {"lines", "DE", "Frankfurt am Main"}} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"-api", url, "-session", session}, args...), &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, stderr.String())
		}
	}

	if got := api.queries["GET /v1/lines/regions"]; got != "country=DE" {
		t.Errorf("regions query %q", got)
	}
	if got := api.queries["GET /v1/lines/available"]; got != "city=Frankfurt+am+Main&country=DE" {
		t.Errorf("lines query %q", got)
	}
}
