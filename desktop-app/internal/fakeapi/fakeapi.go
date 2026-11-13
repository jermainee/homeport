// Package fakeapi is an in-memory stand-in for the Homeport control API's
// member and share endpoints, for tests. It follows
// contracts/control-api.openapi.yaml: the app creates the session token, the
// API sees only its SHA-256, and the token works once the member approves the
// sign-in. Lines need a verified identity, as in the API.
package fakeapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"
)

// Server is a running fake API. Its URL is the API base URL.
type Server struct {
	*httptest.Server

	mu          sync.Mutex
	nextCode    int
	logins      map[string]*login // by login_request_id
	byRequestID map[string]*login
	members     map[string]*Member // by email
	sessions    map[string]*Member // by token hash, active sessions only
	failures    []failure
	offline     bool     // drop every connection, as an unreachable API would
	requests    []string // "METHOD path" of every request, in order

	gateway     *Gateway           // the gateway new lines get, or nil
	devices     map[string]*Device // by device_id
	lines       map[string]*Line   // by line_id
	shareReplay map[string]replay  // POST answers by member and request_id
}

// Gateway is where a line's agent dials out to, as POST /v1/lines returns
// it: the pin is the hex SHA-256 of the gateway certificate's SPKI.
type Gateway struct {
	QUICEndpoint string `json:"quic_endpoint"`
	ServerName   string `json:"server_name"`
	SPKISHA256   string `json:"spki_sha256"`
}

// Device is a registered sharing agent key.
type Device struct {
	ID             string
	MemberID       string
	Name           string
	Platform       string
	AgentPublicKey string // standard base64, as the app sent it
	SPKISHA256     string
}

// Line is a device's line.
type Line struct {
	ID             string
	DeviceID       string
	MemberID       string
	SharingEnabled bool
}

type replay struct {
	body   string // the request body, to detect a conflicting reuse
	status int
	answer any
}

// Member is a member as GET /v1/me returns it.
type Member struct {
	MemberID       string  `json:"member_id"`
	Email          string  `json:"email"`
	IdentityStatus string  `json:"identity_status"`
	Credits        Credits `json:"credits"`
}

// Credits is the member's balance by kind.
type Credits struct {
	Purchased      int64 `json:"purchased"`
	EarnedPending  int64 `json:"earned_pending"`
	EarnedCashable int64 `json:"earned_cashable"`
}

type login struct {
	id         string
	requestID  string
	body       loginRequestBody
	matchCode  string
	expiresAt  time.Time
	approved   bool
	expired    bool
	memberID   string
	sessionExp time.Time
}

type loginRequestBody struct {
	RequestID          string `json:"request_id"`
	Email              string `json:"email"`
	SessionTokenSHA256 string `json:"session_token_sha256"`
}

type failure struct {
	route  string // "METHOD /path-prefix"
	status int
	code   string // error code; empty drops the connection instead
	lose   bool   // handle the request, then drop the connection
	hold   chan struct{}
	date   time.Time // Date header to send with the error, if set
}

var (
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	uuidV7    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// New starts a fake API and stops it when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		nextCode:    482913,
		logins:      map[string]*login{},
		byRequestID: map[string]*login{},
		members:     map[string]*Member{},
		sessions:    map[string]*Member{},
		devices:     map[string]*Device{},
		lines:       map[string]*Line{},
		shareReplay: map[string]replay{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/login-requests", s.createLoginRequest)
	mux.HandleFunc("GET /v1/auth/login-requests/{id}", s.getLoginRequest)
	mux.HandleFunc("GET /v1/me", s.getMe)
	mux.HandleFunc("DELETE /v1/auth/session", s.deleteSession)
	mux.HandleFunc("POST /v1/devices", s.registerDevice)
	mux.HandleFunc("POST /v1/lines", s.createLine)
	mux.HandleFunc("PATCH /v1/lines/{id}", s.updateLine)
	s.Server = httptest.NewServer(s.injectFailures(mux))
	t.Cleanup(s.Close)
	return s
}

// Approve does what the member does on the page behind the email link: it
// approves the most recent sign-in for email and activates its token.
func (s *Server) Approve(t testing.TB, email string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.latestLocked(email)
	if l == nil {
		t.Fatalf("fakeapi: no login request for %s", email)
	}
	m, ok := s.members[email]
	if !ok {
		m = &Member{MemberID: uuid.NewV7().String(), Email: email, IdentityStatus: "unverified"}
		s.members[email] = m
	}
	l.approved = true
	l.memberID = m.MemberID
	l.sessionExp = time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	s.sessions[l.body.SessionTokenSHA256] = m
}

// Expire lets the most recent sign-in for email run out before approval.
func (s *Server) Expire(t testing.TB, email string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.latestLocked(email)
	if l == nil {
		t.Fatalf("fakeapi: no login request for %s", email)
	}
	l.expired = true
}

// RevokeAll ends every active session, as if each was signed out elsewhere.
func (s *Server) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.sessions)
}

// ActiveSessions counts sessions whose tokens work.
func (s *Server) ActiveSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// LoginRequests returns how many distinct login requests were created.
func (s *Server) LoginRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.logins)
}

// MatchCode returns the match code of the most recent sign-in for email.
func (s *Server) MatchCode(t testing.TB, email string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.latestLocked(email)
	if l == nil {
		t.Fatalf("fakeapi: no login request for %s", email)
	}
	return l.matchCode
}

// Requests returns "METHOD path" for each request the fake has served.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// HoldNext makes the next request whose "METHOD path" starts with route
// wait until release is called, then handles it normally.
func (s *Server) HoldNext(route string) (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan struct{})
	s.failures = append(s.failures, failure{route: route, hold: ch})
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// FailNextAt is FailNext with the API's clock at serverTime, as its Date
// header shows.
func (s *Server) FailNextAt(route string, status int, code string, serverTime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, failure{route: route, status: status, code: code, date: serverTime})
}

// LoseNextResponse makes the API handle the next request whose
// "METHOD path" starts with route, then drop the connection before it
// answers, as when the network fails mid-request.
func (s *Server) LoseNextResponse(route string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, failure{route: route, lose: true})
}

// SetOffline makes the API unreachable, dropping every connection, until it
// is called with false. Unlike FailNext, it also defeats the HTTP client's
// own retry of a GET on a dropped keep-alive connection.
func (s *Server) SetOffline(offline bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offline = offline
}

// FailNext makes the next request whose "METHOD path" starts with route
// answer status with {error: code}. An empty code drops the connection, as
// an unreachable API would.
func (s *Server) FailNext(route string, status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, failure{route: route, status: status, code: code})
}

func (s *Server) latestLocked(email string) *login {
	var latest *login
	for _, l := range s.logins {
		if l.body.Email == email && (latest == nil || l.id > latest.id) {
			latest = l
		}
	}
	return latest
}

func (s *Server) injectFailures(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		s.mu.Lock()
		s.requests = append(s.requests, route)
		var f *failure
		for i := range s.failures {
			if strings.HasPrefix(route, s.failures[i].route) {
				ff := s.failures[i]
				f = &ff
				s.failures = append(s.failures[:i], s.failures[i+1:]...)
				break
			}
		}
		offline := s.offline
		s.mu.Unlock()
		if offline {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		if f == nil {
			next.ServeHTTP(w, r)
			return
		}
		if f.hold != nil {
			<-f.hold
			next.ServeHTTP(w, r)
			return
		}
		if f.lose {
			next.ServeHTTP(httptest.NewRecorder(), r)
		}
		if f.code == "" || f.lose {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		if !f.date.IsZero() {
			w.Header().Set("Date", f.date.UTC().Format(http.TimeFormat))
		}
		writeError(w, f.status, f.code)
	})
}

func (s *Server) createLoginRequest(w http.ResponseWriter, r *http.Request) {
	var body loginRequestBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil ||
		!uuidV7.MatchString(body.RequestID) ||
		!sha256Hex.MatchString(body.SessionTokenSHA256) ||
		!strings.Contains(body.Email, "@") {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.byRequestID[body.RequestID]; ok {
		if l.body != body {
			writeError(w, http.StatusConflict, "idempotency_conflict")
			return
		}
		writeLoginRequest(w, l)
		return
	}
	for _, l := range s.logins {
		if l.body.SessionTokenSHA256 == body.SessionTokenSHA256 {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	l := &login{
		id:        uuid.NewV7().String(),
		requestID: body.RequestID,
		body:      body,
		matchCode: fmt.Sprintf("%06d", s.nextCode%1000000),
		expiresAt: time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second),
	}
	s.nextCode += 104729
	s.logins[l.id] = l
	s.byRequestID[l.requestID] = l
	writeLoginRequest(w, l)
}

func writeLoginRequest(w http.ResponseWriter, l *login) {
	writeJSON(w, http.StatusCreated, map[string]any{
		"login_request_id": l.id,
		"match_code":       l.matchCode,
		"expires_at":       l.expiresAt.Format(time.RFC3339),
	})
}

func (s *Server) getLoginRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	hash, ok := bearerHash(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.logins[r.PathValue("id")]
	if !ok || l.body.SessionTokenSHA256 != hash {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	switch {
	case l.expired:
		writeError(w, http.StatusGone, "login_request_expired")
	case l.approved:
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "approved",
			"member_id":  l.memberID,
			"expires_at": l.sessionExp.Format(time.RFC3339),
		})
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
	}
}

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) {
	m := s.member(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	hash, ok := bearerHash(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, active := s.sessions[hash]; !ok || !active {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	delete(s.sessions, hash)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) member(r *http.Request) *Member {
	hash, ok := bearerHash(r)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.sessions[hash]
	if !ok {
		return nil
	}
	cp := *m
	return &cp
}

func bearerHash(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(token, "hpm_") {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]), true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
