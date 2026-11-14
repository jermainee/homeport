// Package fakeapi is an in-memory control API for the gateway's side of the
// contract: the /v1/gateway/* paths of contracts/control-api.openapi.yaml.
//
// It checks every request against the contract (field names and formats,
// required and unknown fields, body limits, idempotency, usage sequences)
// and records what it finds as violations. It answers the way the contract
// describes, with lease answers a test chooses, and it records everything
// the gateway reported, so the end-to-end tests can compare the reports with
// the meters. Its answers are checked against contracts/examples/gateway in
// its own tests.
package fakeapi

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Paths of the gateway's endpoints.
const (
	PathSelf       = "/v1/gateway/self"
	PathConfig     = "/v1/gateway/config"
	PathLeases     = "/v1/gateway/leases"
	PathUsage      = "/v1/gateway/usage"
	PathLineEvents = "/v1/gateway/line-events"
)

// Body limits (x-max-body-bytes).
const (
	smallBody = 16 << 10
	batchBody = 128 << 10
	maxBatch  = 100
)

// Config is the body of GET /v1/gateway/config.
type Config struct {
	ETag            string   `json:"etag"`
	TopUpHostnames  []string `json:"topup_hostnames"`
	FailbackSeconds int      `json:"failback_seconds"`
	Peers           []Peer   `json:"peers"`
	Lines           []Line   `json:"lines"`
}

// Peer is a member device the gateway serves. FallbackLineIDs are the
// lines that take over while LineID is offline, best first; nil is sent as
// an empty list.
type Peer struct {
	PeerID          string   `json:"peer_id"`
	MemberID        string   `json:"member_id"`
	PublicKey       string   `json:"public_key"`
	IPv4            string   `json:"ipv4"`
	IPv6            string   `json:"ipv6"`
	LineID          string   `json:"line_id"`
	FallbackLineIDs []string `json:"fallback_line_ids"`
}

// Line is a shared line whose agent may connect.
type Line struct {
	LineID           string `json:"line_id"`
	OwnerMemberID    string `json:"owner_member_id"`
	DeviceSPKISHA256 string `json:"device_spki_sha256"`
}

// GatewaySelf is the body of PUT /v1/gateway/self.
type GatewaySelf struct {
	WGPublicKey    string `json:"wg_public_key"`
	WGEndpoint     string `json:"wg_endpoint"`
	QUICEndpoint   string `json:"quic_endpoint"`
	QUICServerName string `json:"quic_server_name"`
	QUICSPKISHA256 string `json:"quic_spki_sha256"`
	Version        string `json:"version"`
}

// LeaseRequest is the body of POST /v1/gateway/leases.
type LeaseRequest struct {
	RequestID         string `json:"request_id"`
	PeerID            string `json:"peer_id"`
	SessionID         string `json:"session_id"`
	LineID            string `json:"line_id,omitempty"`
	SupersedesLeaseID string `json:"supersedes_lease_id,omitempty"`
}

// Lease is a granted lease.
type Lease struct {
	LeaseID      string `json:"lease_id"`
	GrantedBytes int64  `json:"granted_bytes"`
	RenewAtBytes int64  `json:"renew_at_bytes"`
	ExpiresAt    string `json:"expires_at"`
	GraceSeconds int    `json:"grace_seconds"`
}

// UsageEvent is one usage event the gateway reported.
type UsageEvent struct {
	EventID      string        `json:"event_id"`
	SessionID    string        `json:"session_id"`
	PeerID       string        `json:"peer_id"`
	LineID       string        `json:"line_id"`
	LeaseID      string        `json:"lease_id"`
	Seq          int           `json:"seq"`
	BytesUp      int64         `json:"bytes_up"`
	BytesDown    int64         `json:"bytes_down"`
	PeriodStart  string        `json:"period_start"`
	PeriodEnd    string        `json:"period_end"`
	Final        bool          `json:"final"`
	SessionStart *SessionStart `json:"session_start,omitempty"`
}

// SessionStart is on a session's first event.
type SessionStart struct {
	StartedAt string `json:"started_at"`
	ExitIP    string `json:"exit_ip"`
}

// LineEvent is one line event the gateway reported.
type LineEvent struct {
	EventID      string `json:"event_id"`
	LineID       string `json:"line_id"`
	Type         string `json:"type"`
	ObservedIP   string `json:"observed_ip"`
	AgentVersion string `json:"agent_version"`
	At           string `json:"at"`
}

// LeaseAnswer is how the API answers a lease request.
type LeaseAnswer struct {
	Status       int // 201, 402 or 403
	GrantedBytes int64
	RenewAtBytes int64
	GraceSeconds int
	TTL          time.Duration // the lease's lifetime; 0 means an hour
}

// Grant grants a lease of granted bytes, renewed at renewAt, with grace
// seconds of top-up-only access once it is used up.
func Grant(granted, renewAt int64, grace int) LeaseAnswer {
	return LeaseAnswer{Status: http.StatusCreated, GrantedBytes: granted, RenewAtBytes: renewAt, GraceSeconds: grace}
}

// Deny answers 402: the member has no credits left.
func Deny(grace int) LeaseAnswer {
	return LeaseAnswer{Status: http.StatusPaymentRequired, GraceSeconds: grace}
}

// Inactive answers 403: the peer may not be used any more.
func Inactive() LeaseAnswer { return LeaseAnswer{Status: http.StatusForbidden} }

// LeasePolicy answers the n-th lease request (from 0, retries of one
// request_id counted once).
type LeasePolicy func(n int, req LeaseRequest) LeaseAnswer

// Production-sized leases: 2,000 credits are 200 MB, renewed at 80%.
func defaultLeases(int, LeaseRequest) LeaseAnswer { return Grant(200_000_000, 160_000_000, 300) }

// Failure is a fault the API injects into the next request on a path.
type Failure int

const (
	// FailWith500 answers 500 without doing anything, as the API does
	// when it fails before storing the request.
	FailWith500 Failure = iota
	// ApplyThenDrop does the request, then drops the connection without
	// an answer, as when the answer is lost on its way back.
	ApplyThenDrop
)

// Call is one request the API received.
type Call struct {
	Seq    int
	At     time.Time
	Method string
	Path   string
	Status int // 0 when the connection was dropped
	// EventIDs are the usage or line events the request carried.
	EventIDs []string
	// Final reports a usage request that carried a final event.
	Final bool
}

// API is the fake control API. It is an http.Handler.
type API struct {
	token string

	mu         sync.Mutex
	config     Config
	leases     LeasePolicy
	failures   map[string][]Failure
	self       *GatewaySelf
	calls      []Call
	leaseReqs  []LeaseRequest           // one per request_id, in order
	answers    map[string]storedAnswer  // by request_id
	granted    map[string]*grantedLease // by lease_id
	usage      []UsageEvent             // applied, in order
	usageByID  map[string]UsageEvent
	sessions   map[string]*sessionState
	lineEvents []LineEvent
	lineByID   map[string]LineEvent
	returns    []string // lease IDs, in order
	returnReqs map[string]string
	violations []string
}

type storedAnswer struct {
	body   []byte // the request, to detect a conflicting reuse
	status int
	answer []byte
}

type grantedLease struct {
	lease        Lease
	peerID       string
	lineID       string
	sessionID    string
	supersededBy string
	returned     bool
}

type sessionState struct {
	peerID, lineID string
	nextSeq        int
	periodEnd      string
	final          bool
}

// New returns an API that accepts token and serves cfg. Leases are
// production-sized until SetLeases says otherwise.
func New(token string, cfg Config) *API {
	return &API{
		token:      token,
		config:     cfg,
		leases:     defaultLeases,
		failures:   map[string][]Failure{},
		answers:    map[string]storedAnswer{},
		granted:    map[string]*grantedLease{},
		usageByID:  map[string]UsageEvent{},
		sessions:   map[string]*sessionState{},
		lineByID:   map[string]LineEvent{},
		returnReqs: map[string]string{},
	}
}

// SetConfig replaces the config the API serves, as a member's change does,
// such as turning Share on. cfg needs an ETag of its own.
func (a *API) SetConfig(cfg Config) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config = cfg
}

// SetLeases sets how lease requests are answered.
func (a *API) SetLeases(p LeasePolicy) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.leases = p
}

// FailNext makes the next request to path fail with f. Calls queue up.
func (a *API) FailNext(path string, f Failure) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures[path] = append(a.failures[path], f)
}

// Self returns the gateway's last announcement.
func (a *API) Self() (GatewaySelf, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.self == nil {
		return GatewaySelf{}, false
	}
	return *a.self, true
}

// Calls returns every request so far, in order.
func (a *API) Calls() []Call {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Call(nil), a.calls...)
}

// LeaseRequests returns the lease requests, one per request_id, in order.
func (a *API) LeaseRequests() []LeaseRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]LeaseRequest(nil), a.leaseReqs...)
}

// Usage returns the applied usage events, each once, in order.
func (a *API) Usage() []UsageEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]UsageEvent(nil), a.usage...)
}

// LineEvents returns the applied line events, each once, in order.
func (a *API) LineEvents() []LineEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]LineEvent(nil), a.lineEvents...)
}

// Returns returns the IDs of the returned leases, in order.
func (a *API) Returns() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.returns...)
}

// Violations returns every way the gateway broke the contract.
func (a *API) Violations() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.violations...)
}

// UsageTotal sums the applied usage events.
func (a *API) UsageTotal() (up, down uint64) {
	for _, e := range a.Usage() {
		up += uint64(e.BytesUp)     // #nosec G115 -- checked to be non-negative
		down += uint64(e.BytesDown) // #nosec G115 -- checked to be non-negative
	}
	return up, down
}

// requestError is a request outside the contract.
type requestError struct {
	status int
	code   string
	msg    string
}

func (e *requestError) Error() string { return e.msg }

func invalid(format string, args ...any) error {
	return &requestError{status: http.StatusBadRequest, code: "invalid_request", msg: fmt.Sprintf(format, args...)}
}

// ServeHTTP serves the gateway's endpoints.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	limit := int64(smallBody)
	if r.URL.Path == PathUsage || r.URL.Path == PathLineEvents {
		limit = batchBody
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	call := Call{Seq: len(a.calls), At: time.Now(), Method: r.Method, Path: r.URL.Path}
	var fail *Failure
	if q := a.failures[r.URL.Path]; len(q) > 0 {
		fail, a.failures[r.URL.Path] = &q[0], q[1:]
	}
	status, answer, header := a.serve(r, body, limit, &call, fail)
	if fail != nil && *fail == ApplyThenDrop {
		a.calls = append(a.calls, call)
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}
	call.Status = status
	a.calls = append(a.calls, call)
	for k, v := range header {
		w.Header()[k] = v
	}
	if answer == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(answer) // #nosec G705 -- JSON this API built, served as application/json
}

// serve handles one request under a.mu and returns the answer.
func (a *API) serve(r *http.Request, body []byte, limit int64, call *Call, fail *Failure) (int, []byte, http.Header) {
	if r.Header.Get("Authorization") != "Bearer "+a.token {
		a.violate("%s %s without the gateway token", r.Method, r.URL.Path)
		return errorAnswer(http.StatusUnauthorized, "unauthorized")
	}
	if int64(len(body)) > limit {
		a.violate("%s %s: body over %d bytes", r.Method, r.URL.Path, limit)
		return errorAnswer(http.StatusRequestEntityTooLarge, "payload_too_large")
	}
	if fail != nil && *fail == FailWith500 {
		call.EventIDs = eventIDs(body)
		return errorAnswer(http.StatusInternalServerError, "internal_error")
	}
	var err error
	var status int
	var answer []byte
	var header http.Header
	switch {
	case r.URL.Path == PathSelf && r.Method == http.MethodPut:
		status, err = a.putSelf(body)
	case r.URL.Path == PathConfig && r.Method == http.MethodGet:
		status, answer, header = a.getConfig(r)
	case r.URL.Path == PathLeases && r.Method == http.MethodPost:
		status, answer, err = a.createLease(body)
	case strings.HasPrefix(r.URL.Path, PathLeases+"/") && strings.HasSuffix(r.URL.Path, "/return") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, PathLeases+"/"), "/return")
		status, answer, err = a.returnLease(id, body)
	case r.URL.Path == PathUsage && r.Method == http.MethodPost:
		status, answer, err = a.reportUsage(body, call)
	case r.URL.Path == PathLineEvents && r.Method == http.MethodPost:
		status, answer, err = a.reportLineEvents(body, call)
	default:
		a.violate("%s %s is not in the contract", r.Method, r.URL.Path)
		return errorAnswer(http.StatusNotFound, "not_found")
	}
	var re *requestError
	if errors.As(err, &re) {
		a.violate("%s %s: %s", r.Method, r.URL.Path, re.msg)
		return errorAnswer(re.status, re.code)
	}
	return status, answer, header
}

func errorAnswer(status int, code string) (int, []byte, http.Header) {
	b, _ := json.Marshal(map[string]string{"error": code})
	return status, b, nil
}

func (a *API) violate(format string, args ...any) {
	a.violations = append(a.violations, fmt.Sprintf(format, args...))
}

func (a *API) putSelf(body []byte) (int, error) {
	var s GatewaySelf
	if err := decodeStrict(body, &s, "wg_public_key", "wg_endpoint", "quic_endpoint", "quic_server_name", "quic_spki_sha256", "version"); err != nil {
		return 0, err
	}
	if err := firstErr(
		match("wg_public_key", s.WGPublicKey, key32),
		match("wg_endpoint", s.WGEndpoint, hostPort),
		match("quic_endpoint", s.QUICEndpoint, hostPort),
		match("quic_server_name", s.QUICServerName, hostname),
		match("quic_spki_sha256", s.QUICSPKISHA256, sha256Hex),
		match("version", s.Version, semver),
	); err != nil {
		return 0, err
	}
	a.self = &s
	return http.StatusNoContent, nil
}

func (a *API) getConfig(r *http.Request) (int, []byte, http.Header) {
	h := http.Header{"Etag": {a.config.ETag}}
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.TrimPrefix(inm, "W/") == a.config.ETag {
		return http.StatusNotModified, nil, h
	}
	cfg := a.config
	cfg.Peers = make([]Peer, len(a.config.Peers))
	for i, p := range a.config.Peers {
		if p.FallbackLineIDs == nil {
			p.FallbackLineIDs = []string{}
		}
		cfg.Peers[i] = p
	}
	b, _ := json.Marshal(cfg)
	return http.StatusOK, b, h
}

func (a *API) createLease(body []byte) (int, []byte, error) {
	var req LeaseRequest
	if err := decodeStrict(body, &req, "request_id", "peer_id", "session_id"); err != nil {
		return 0, nil, err
	}
	if err := firstErr(
		match("request_id", req.RequestID, uuidV7),
		match("peer_id", req.PeerID, id),
		match("session_id", req.SessionID, uuidV7),
	); err != nil {
		return 0, nil, err
	}
	if req.SupersedesLeaseID != "" {
		if err := match("supersedes_lease_id", req.SupersedesLeaseID, id); err != nil {
			return 0, nil, err
		}
	}
	if req.LineID != "" {
		if err := match("line_id", req.LineID, id); err != nil {
			return 0, nil, err
		}
	}
	if stored, ok := a.answers[req.RequestID]; ok {
		if !bytes.Equal(stored.body, body) {
			return 0, nil, &requestError{status: http.StatusConflict, code: "idempotency_conflict", msg: "request_id reused with another body"}
		}
		return stored.status, stored.answer, nil
	}
	peer, ok := a.peer(req.PeerID)
	if !ok {
		return 0, nil, invalid("lease for unknown peer %s", req.PeerID)
	}
	lineID := req.LineID
	if old := req.SupersedesLeaseID; old != "" {
		prev, ok := a.granted[old]
		if !ok {
			return 0, nil, &requestError{status: http.StatusNotFound, code: "not_found", msg: "renewal of a lease the API never granted"}
		}
		if prev.sessionID != req.SessionID || prev.peerID != req.PeerID {
			return 0, nil, invalid("renewal of lease %s from another session", old)
		}
		if lineID == "" {
			lineID = prev.lineID
		}
		if lineID != prev.lineID {
			return 0, nil, invalid("renewal of lease %s on line %s, not its line %s", old, lineID, prev.lineID)
		}
		if next := prev.supersededBy; next != "" {
			b, _ := json.Marshal(a.granted[next].lease)
			a.answers[req.RequestID] = storedAnswer{body: body, status: http.StatusOK, answer: b}
			return http.StatusOK, b, nil
		}
	}
	if lineID == "" {
		lineID = peer.LineID
	}
	ans := a.leases(len(a.leaseReqs), req)
	if !slices.Contains(append([]string{peer.LineID}, peer.FallbackLineIDs...), lineID) {
		// The API refuses a line that is not (or no longer) one of the
		// peer's, as after a config change the gateway has not fetched yet.
		ans = Inactive()
	}
	a.leaseReqs = append(a.leaseReqs, req)
	var status int
	var answer []byte
	switch ans.Status {
	case http.StatusCreated:
		ttl := ans.TTL
		if ttl == 0 {
			ttl = time.Hour
		}
		l := Lease{
			LeaseID:      newID(),
			GrantedBytes: ans.GrantedBytes,
			RenewAtBytes: ans.RenewAtBytes,
			ExpiresAt:    time.Now().Add(ttl).UTC().Format(time.RFC3339),
			GraceSeconds: ans.GraceSeconds,
		}
		a.granted[l.LeaseID] = &grantedLease{lease: l, peerID: req.PeerID, lineID: lineID, sessionID: req.SessionID}
		if old := req.SupersedesLeaseID; old != "" {
			a.granted[old].supersededBy = l.LeaseID
		}
		status = http.StatusCreated
		answer, _ = json.Marshal(l)
	case http.StatusPaymentRequired:
		status = ans.Status
		answer, _ = json.Marshal(map[string]any{"error": "insufficient_credits", "grace_seconds": ans.GraceSeconds})
	default:
		status = http.StatusForbidden
		answer, _ = json.Marshal(map[string]any{"error": "peer_inactive"})
	}
	a.answers[req.RequestID] = storedAnswer{body: body, status: status, answer: answer}
	return status, answer, nil
}

func (a *API) returnLease(leaseID string, body []byte) (int, []byte, error) {
	var req struct {
		RequestID string `json:"request_id"`
	}
	if err := decodeStrict(body, &req, "request_id"); err != nil {
		return 0, nil, err
	}
	if err := firstErr(match("lease_id", leaseID, id), match("request_id", req.RequestID, uuidV7)); err != nil {
		return 0, nil, err
	}
	if prev, ok := a.returnReqs[req.RequestID]; ok && prev != leaseID {
		return 0, nil, &requestError{status: http.StatusConflict, code: "idempotency_conflict", msg: "request_id reused for another lease"}
	}
	l, ok := a.granted[leaseID]
	if !ok {
		return 0, nil, &requestError{status: http.StatusNotFound, code: "not_found", msg: "return of a lease the API never granted"}
	}
	if s := a.sessions[l.sessionID]; s != nil && !s.final {
		return 0, nil, invalid("lease %s returned before its session's final usage event", leaseID)
	}
	a.returnReqs[req.RequestID] = leaseID
	if !l.returned {
		l.returned = true
		a.returns = append(a.returns, leaseID)
	}
	// released_credits is for logs only; this fake holds no prices.
	return http.StatusOK, []byte(`{"released_credits":0}`), nil
}

func (a *API) reportUsage(body []byte, call *Call) (int, []byte, error) {
	events, err := decodeBatch[UsageEvent](body, "event_id", "session_id", "peer_id", "line_id", "lease_id", "seq", "bytes_up", "bytes_down", "period_start", "period_end", "final")
	if err != nil {
		return 0, nil, err
	}
	for i, e := range events {
		if err := checkUsageEvent(e); err != nil {
			return 0, nil, invalid("event %d: %v", i, err)
		}
		call.EventIDs = append(call.EventIDs, e.EventID)
		call.Final = call.Final || e.Final
	}
	results := make([]map[string]string, len(events))
	for i, e := range events {
		results[i] = map[string]string{"event_id": e.EventID, "status": a.applyUsage(e)}
		if results[i]["status"] != "applied" && results[i]["status"] != "duplicate" {
			a.violate("usage event %s rejected: %s", e.EventID, results[i]["status"])
			results[i]["reason"] = results[i]["status"]
			results[i]["status"] = "rejected"
		}
	}
	b, _ := json.Marshal(map[string]any{"results": results})
	return http.StatusOK, b, nil
}

// applyUsage applies one valid event and returns "applied", "duplicate" or
// the reason to reject it.
func (a *API) applyUsage(e UsageEvent) string {
	if prev, ok := a.usageByID[e.EventID]; ok {
		if !sameEvent(prev, e) {
			return "seq_conflict" // an event_id reused for other bytes
		}
		return "duplicate"
	}
	if !a.knownPeer(e.PeerID) {
		return "unknown_peer"
	}
	if !a.knownLine(e.LineID) {
		return "unknown_line"
	}
	l, ok := a.granted[e.LeaseID]
	if !ok {
		return "unknown_lease"
	}
	if l.peerID != e.PeerID || l.sessionID != e.SessionID || l.lineID != e.LineID {
		return "lease_mismatch"
	}
	s := a.sessions[e.SessionID]
	if s == nil {
		s = &sessionState{peerID: e.PeerID, lineID: e.LineID}
	}
	switch {
	case e.Seq != s.nextSeq || s.final || s.peerID != e.PeerID || s.lineID != e.LineID:
		return "seq_conflict"
	case s.periodEnd != "" && e.PeriodStart != s.periodEnd:
		return "invalid_period"
	}
	s.nextSeq++
	s.periodEnd = e.PeriodEnd
	s.final = e.Final
	a.sessions[e.SessionID] = s
	a.usageByID[e.EventID] = e
	a.usage = append(a.usage, e)
	return "applied"
}

func sameEvent(a, b UsageEvent) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func checkUsageEvent(e UsageEvent) error {
	if err := firstErr(
		match("event_id", e.EventID, uuidV7),
		match("session_id", e.SessionID, uuidV7),
		match("peer_id", e.PeerID, id),
		match("line_id", e.LineID, id),
		match("lease_id", e.LeaseID, id),
	); err != nil {
		return err
	}
	switch {
	case e.Seq < 0 || e.BytesUp < 0 || e.BytesDown < 0:
		return errors.New("negative seq or byte count")
	case (e.Seq == 0) != (e.SessionStart != nil):
		return errors.New("session_start must be on seq 0 and only there")
	}
	start, err1 := time.Parse(time.RFC3339, e.PeriodStart)
	end, err2 := time.Parse(time.RFC3339, e.PeriodEnd)
	if err1 != nil || err2 != nil {
		return errors.New("period_start and period_end must be RFC 3339 timestamps")
	}
	if !end.After(start) {
		return errors.New("period_end must be later than period_start")
	}
	if s := e.SessionStart; s != nil {
		if _, err := time.Parse(time.RFC3339, s.StartedAt); err != nil {
			return errors.New("session_start.started_at is not a timestamp")
		}
		if _, err := netip.ParseAddr(s.ExitIP); err != nil {
			return errors.New("session_start.exit_ip is not an IP address")
		}
	}
	return nil
}

func (a *API) reportLineEvents(body []byte, call *Call) (int, []byte, error) {
	events, err := decodeBatch[LineEvent](body, "event_id", "line_id", "type", "observed_ip", "agent_version", "at")
	if err != nil {
		return 0, nil, err
	}
	for i, e := range events {
		err := firstErr(match("event_id", e.EventID, uuidV7), match("line_id", e.LineID, id), match("agent_version", e.AgentVersion, semver))
		if err == nil && e.Type != "online" && e.Type != "offline" {
			err = errors.New("type must be online or offline")
		}
		if _, perr := netip.ParseAddr(e.ObservedIP); err == nil && perr != nil {
			err = errors.New("observed_ip is not an IP address")
		}
		if _, terr := time.Parse(time.RFC3339, e.At); err == nil && terr != nil {
			err = errors.New("at is not a timestamp")
		}
		if err != nil {
			return 0, nil, invalid("line event %d: %v", i, err)
		}
		call.EventIDs = append(call.EventIDs, e.EventID)
	}
	results := make([]map[string]string, len(events))
	for i, e := range events {
		status := "applied"
		switch prev, seen := a.lineByID[e.EventID]; {
		case seen && prev != e:
			a.violate("line event %s reused with another body", e.EventID)
			status = "rejected"
		case seen:
			status = "duplicate"
		case !a.knownLine(e.LineID):
			a.violate("line event for unknown line %s", e.LineID)
			status = "rejected"
		default:
			a.lineByID[e.EventID] = e
			a.lineEvents = append(a.lineEvents, e)
		}
		results[i] = map[string]string{"event_id": e.EventID, "status": status}
		if status == "rejected" {
			results[i]["reason"] = "unknown_line"
		}
	}
	b, _ := json.Marshal(map[string]any{"results": results})
	return http.StatusOK, b, nil
}

func (a *API) knownPeer(peerID string) bool {
	_, ok := a.peer(peerID)
	return ok
}

func (a *API) peer(peerID string) (Peer, bool) {
	for _, p := range a.config.Peers {
		if p.PeerID == peerID {
			return p, true
		}
	}
	return Peer{}, false
}

func (a *API) knownLine(lineID string) bool {
	for _, l := range a.config.Lines {
		if l.LineID == lineID {
			return true
		}
	}
	return false
}

// eventIDs returns the event IDs in a batch, without checking it.
func eventIDs(body []byte) []string {
	var batch struct {
		Events []struct {
			EventID string `json:"event_id"`
		} `json:"events"`
	}
	_ = json.Unmarshal(body, &batch)
	var ids []string
	for _, e := range batch.Events {
		ids = append(ids, e.EventID)
	}
	return ids
}

// decodeStrict decodes one JSON object into v. Every required key must be
// present, and no key outside v's fields is allowed.
func decodeStrict(body []byte, v any, required ...string) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return invalid("body is not a JSON object: %v", err)
	}
	for _, k := range required {
		if _, ok := raw[k]; !ok {
			return invalid("missing %s", k)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("%v", err)
	}
	return nil
}

// decodeBatch decodes {"events": [...]} with 1 to 100 events, each checked
// like decodeStrict.
func decodeBatch[T any](body []byte, required ...string) ([]T, error) {
	var batch struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := decodeStrict(body, &batch, "events"); err != nil {
		return nil, err
	}
	if n := len(batch.Events); n < 1 || n > maxBatch {
		return nil, invalid("%d events in a batch, want 1 to %d", n, maxBatch)
	}
	out := make([]T, len(batch.Events))
	for i, raw := range batch.Events {
		if err := decodeStrict(raw, &out[i], required...); err != nil {
			return nil, invalid("event %d: %v", i, err)
		}
	}
	return out, nil
}

// Formats from the contract's schemas.
var (
	id        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	uuidV7    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	key32     = regexp.MustCompile(`^[A-Za-z0-9+/]{42}[AEIMQUYcgkosw480]=$`)
	hostPort  = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+):[0-9]{1,5}$`)
	hostname  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	semver    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
)

func match(field, v string, re *regexp.Regexp) error {
	if !re.MatchString(v) {
		return invalid("%s %q does not match %s", field, v, re)
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// newID returns a lowercase random UUID, like the API's resource IDs.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
