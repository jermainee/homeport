package fakeapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"testing"
	"uuid"
)

var key32 = regexp.MustCompile(`^[A-Za-z0-9+/]{42}[AEIMQUYcgkosw480]=$`)

// Verify marks email's identity as verified, as the Stripe Identity webhook
// does. Lines need a verified identity.
func (s *Server) Verify(t testing.TB, email string) {
	t.Helper()
	s.SetIdentityStatus(t, email, "verified")
}

// SetIdentityStatus sets email's identity status: unverified,
// requires_input, processing or verified.
func (s *Server) SetIdentityStatus(t testing.TB, email, status string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.members[email]
	if !ok {
		t.Fatalf("fakeapi: no member %s", email)
	}
	m.IdentityStatus = status
}

// SetGateway sets the gateway that lines get from now on, and moves every
// existing line to it, as a gateway that announced new keys would. Without
// one, POST /v1/lines answers 503 gateway_unavailable.
func (s *Server) SetGateway(g Gateway) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gateway = &g
}

// Devices returns the devices email registered, oldest first.
func (s *Server) Devices(t testing.TB, email string) []Device {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.members[email]
	if m == nil {
		t.Fatalf("fakeapi: no member %s", email)
	}
	var out []Device
	for _, d := range s.devices {
		if d.MemberID == m.MemberID {
			out = append(out, *d)
		}
	}
	slices.SortFunc(out, func(a, b Device) int { return compare(a.ID, b.ID) })
	return out
}

// RemoveDevice deletes a device and its line, as support would for a lost
// Mac.
func (s *Server) RemoveDevice(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devices, deviceID)
	for id, l := range s.lines {
		if l.DeviceID == deviceID {
			delete(s.lines, id)
		}
	}
}

// Lines returns the lines email owns, oldest first.
func (s *Server) Lines(t testing.TB, email string) []Line {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.members[email]
	if m == nil {
		t.Fatalf("fakeapi: no member %s", email)
	}
	var out []Line
	for _, l := range s.lines {
		if l.MemberID == m.MemberID {
			out = append(out, *l)
		}
	}
	slices.SortFunc(out, func(a, b Line) int { return compare(a.ID, b.ID) })
	return out
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// decodeStrict reads a JSON object with only the fields of v, as the API's
// closed schemas require. It returns the raw body for idempotency checks.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16384))
	if err != nil {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, false
	}
	return raw, true
}

// replayLocked answers a POST whose request_id the member used before: the
// first answer again for the same body, 409 for another body.
func (s *Server) replayLocked(w http.ResponseWriter, m *Member, requestID string, body []byte) bool {
	prev, ok := s.shareReplay[m.MemberID+"/"+requestID]
	if !ok {
		return false
	}
	if prev.body != string(body) {
		writeError(w, http.StatusConflict, "idempotency_conflict")
		return true
	}
	writeJSON(w, prev.status, prev.answer)
	return true
}

func (s *Server) answerLocked(w http.ResponseWriter, m *Member, requestID string, body []byte, status int, answer any) {
	s.shareReplay[m.MemberID+"/"+requestID] = replay{body: string(body), status: status, answer: answer}
	writeJSON(w, status, answer)
}

func (s *Server) registerDevice(w http.ResponseWriter, r *http.Request) {
	m := s.memberRef(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		RequestID      string `json:"request_id"`
		Name           string `json:"name"`
		Platform       string `json:"platform"`
		AgentPublicKey string `json:"agent_public_key"`
	}
	raw, ok := decodeStrict(w, r, &body)
	if !ok || !uuidV7.MatchString(body.RequestID) ||
		len([]rune(body.Name)) < 1 || len([]rune(body.Name)) > 64 ||
		!slices.Contains([]string{"linux", "macos", "windows"}, body.Platform) ||
		!key32.MatchString(body.AgentPublicKey) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	pub, _ := base64.StdEncoding.DecodeString(body.AgentPublicKey)
	spki, err := x509.MarshalPKIXPublicKey(ed25519.PublicKey(pub))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	sum := sha256.Sum256(spki)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayLocked(w, m, body.RequestID, raw) {
		return
	}
	for _, d := range s.devices {
		if d.AgentPublicKey != body.AgentPublicKey {
			continue
		}
		if d.MemberID != m.MemberID {
			writeError(w, http.StatusConflict, "agent_key_taken")
			return
		}
		s.answerLocked(w, m, body.RequestID, raw, http.StatusOK, deviceAnswer(d))
		return
	}
	d := &Device{
		ID:             uuid.NewV7().String(),
		MemberID:       m.MemberID,
		Name:           body.Name,
		Platform:       body.Platform,
		AgentPublicKey: body.AgentPublicKey,
		SPKISHA256:     hex.EncodeToString(sum[:]),
	}
	s.devices[d.ID] = d
	s.answerLocked(w, m, body.RequestID, raw, http.StatusCreated, deviceAnswer(d))
}

func deviceAnswer(d *Device) map[string]string {
	return map[string]string{"device_id": d.ID, "spki_sha256": d.SPKISHA256}
}

func (s *Server) createLine(w http.ResponseWriter, r *http.Request) {
	m := s.memberRef(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.verified(m) {
		writeError(w, http.StatusForbidden, "identity_required")
		return
	}
	var body struct {
		RequestID string `json:"request_id"`
		DeviceID  string `json:"device_id"`
	}
	raw, ok := decodeStrict(w, r, &body)
	if !ok || !uuidV7.MatchString(body.RequestID) || body.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayLocked(w, m, body.RequestID, raw) {
		return
	}
	d, ok := s.devices[body.DeviceID]
	if !ok || d.MemberID != m.MemberID {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if s.gateway == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable")
		return
	}
	for _, l := range s.lines {
		if l.DeviceID == d.ID {
			s.answerLocked(w, m, body.RequestID, raw, http.StatusOK, s.lineAnswerLocked(l))
			return
		}
	}
	l := &Line{ID: uuid.NewV7().String(), DeviceID: d.ID, MemberID: m.MemberID}
	s.lines[l.ID] = l
	s.answerLocked(w, m, body.RequestID, raw, http.StatusCreated, s.lineAnswerLocked(l))
}

func (s *Server) lineAnswerLocked(l *Line) map[string]any {
	return map[string]any{"line_id": l.ID, "gateway": *s.gateway}
}

func (s *Server) updateLine(w http.ResponseWriter, r *http.Request) {
	m := s.memberRef(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.verified(m) {
		writeError(w, http.StatusForbidden, "identity_required")
		return
	}
	var body struct {
		SharingEnabled *bool `json:"sharing_enabled"`
	}
	if _, ok := decodeStrict(w, r, &body); !ok || body.SharingEnabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lines[r.PathValue("id")]
	if !ok || l.MemberID != m.MemberID {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	l.SharingEnabled = *body.SharingEnabled
	writeJSON(w, http.StatusOK, map[string]any{"line_id": l.ID, "sharing_enabled": l.SharingEnabled})
}

// memberRef returns the member behind the request's session, not a copy.
func (s *Server) memberRef(r *http.Request) *Member {
	hash, ok := bearerHash(r)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[hash]
}

func (s *Server) verified(m *Member) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return m.IdentityStatus == "verified"
}
