package controlplane_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
	"github.com/jermainee/homeport/gateway/internal/controlplane"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

const (
	testToken = "hpgw_01a1c4af-0000-7000-8000-000000000001.secret" // #nosec G101 -- a fake API's token

	// From contracts/examples/gateway/config.json.
	peerAID  = "01a1c4af-89c1-7c5b-a49a-3d557fc869e7"
	peerAKey = "ID8qVleL1sxlKXgzZQbzTPuAdlP3h/m37nfq8+IB9DA="
	peerBID  = "01a1c4af-8896-7060-852d-c68d8a6174fc"
	peerBKey = "6jNt20ZEki0n+WFznHNpE+YaFSCY/YH0HXDb5uklklU="
	memberID = "01a1c4af-8795-7a28-9249-adbd2ba1c04a"
	lineID   = "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e6"
	ownerID  = "01a1c4af-897b-7f0a-a915-c1ba8ba7b153"
	linePin  = "9cbcb6d9cf5e797e8b18a940a0fd2a645edc317d1dc5f272a988223ae69cc2f8"

	// Peer A's fallback line in the example config, and its owner.
	fallbackLineID = "01a1c4af-8c3e-7d21-9b4a-6f0e2d9c1a57"
	fallbackOwner  = "01a1c4af-8d52-7b10-8e6f-2a4c7b9d3e18"
)

// The contract's patterns, which the fake API checks every request against.
var (
	uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	idPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
)

// example reads a JSON example from the contract.
func example(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "examples", "gateway", name)) // #nosec G304 -- the contract's examples
	if err != nil {
		t.Fatalf("read contract example: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("parse contract example %s: %v", name, err)
	}
	return v
}

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// usageEvent is a usage event as the fake API receives it.
type usageEvent struct {
	EventID      string `json:"event_id"`
	SessionID    string `json:"session_id"`
	PeerID       string `json:"peer_id"`
	LineID       string `json:"line_id"`
	LeaseID      string `json:"lease_id"`
	Seq          int    `json:"seq"`
	BytesUp      uint64 `json:"bytes_up"`
	BytesDown    uint64 `json:"bytes_down"`
	PeriodStart  string `json:"period_start"`
	PeriodEnd    string `json:"period_end"`
	Final        bool   `json:"final"`
	SessionStart *struct {
		StartedAt string `json:"started_at"`
		ExitIP    string `json:"exit_ip"`
	} `json:"session_start"`
}

func (e usageEvent) bytes() uint64 { return e.BytesUp + e.BytesDown }

type leaseRequest struct {
	RequestID         string `json:"request_id"`
	PeerID            string `json:"peer_id"`
	SessionID         string `json:"session_id"`
	LineID            string `json:"line_id"`
	SupersedesLeaseID string `json:"supersedes_lease_id"`
}

type lineEvent struct {
	EventID      string `json:"event_id"`
	LineID       string `json:"line_id"`
	Type         string `json:"type"`
	ObservedIP   string `json:"observed_ip"`
	AgentVersion string `json:"agent_version"`
	At           string `json:"at"`
}

// apiCall is one request the fake API received, in order.
type apiCall struct {
	op   string // "lease", "return", "usage", "line-events", "config", "self"
	body string
}

// fakeAPI is the control API as the contract describes it, serving the
// contract's examples.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	config     map[string]any
	configTags []string // If-None-Match of each config request
	calls      []apiCall
	self       map[string]any
	leases     []leaseRequest
	nextLease  int
	leaseFn    func(r leaseRequest) (status int, body any)
	events     []usageEvent // every usage event received, retries included
	usageFails int          // answer this many usage requests with 503
	clock      *fakeClock
	offset     time.Duration // the API's clock minus the gateway's
	rejectSeq  map[int]bool  // usage events with these seqs are rejected
	returns    []string      // returned lease IDs, in order
	lineEvents []lineEvent
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	api := &fakeAPI{t: t, config: example(t, "config.json"), rejectSeq: map[int]bool{}}
	api.leaseFn = api.grant
	api.srv = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.srv.Close)
	return api
}

// grant answers a lease request with the contract's example lease and a
// fresh lease ID.
func (a *fakeAPI) grant(leaseRequest) (int, any) {
	a.nextLease++
	l := example(a.t, "lease-granted.json")
	l["lease_id"] = leaseID(a.nextLease)
	// The example expires at 10:00; each later lease an hour after the
	// previous one. Both on the API's clock.
	l["expires_at"] = time.Date(2026, 11, 13, 9+a.nextLease, 0, 0, 0, time.UTC).Add(a.offset).Format(time.RFC3339)
	return http.StatusCreated, l
}

func leaseID(n int) string { return fmt.Sprintf("01a1d424-0d22-7c3e-9f5a-%012d", n) }

func (a *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.clock != nil {
		w.Header().Set("Date", a.clock.Now().Add(a.offset).UTC().Format(http.TimeFormat))
	}
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/v1/gateway/self":
		a.record("self", body)
		a.self = decode[map[string]any](a.t, body)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/gateway/config":
		a.record("config", body)
		tag := a.config["etag"].(string)
		a.configTags = append(a.configTags, r.Header.Get("If-None-Match"))
		if inm := r.Header.Get("If-None-Match"); inm == tag || inm == "W/"+tag {
			w.Header().Set("ETag", "W/"+tag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", "W/"+tag) // as Cloudflare weakens it
		writeJSON(w, http.StatusOK, a.config)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/gateway/leases":
		a.record("lease", body)
		req := decode[leaseRequest](a.t, body)
		a.checkLeaseRequest(req)
		a.leases = append(a.leases, req)
		status, resp := a.leaseFn(req)
		writeJSON(w, status, resp)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/gateway/leases/") && strings.HasSuffix(r.URL.Path, "/return"):
		a.record("return", body)
		req := decode[map[string]string](a.t, body)
		if !uuidV7Pattern.MatchString(req["request_id"]) {
			a.t.Errorf("lease return request_id %q is not a UUIDv7", req["request_id"])
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/gateway/leases/"), "/return")
		a.returns = append(a.returns, id)
		writeJSON(w, http.StatusOK, example(a.t, "lease-returned.json"))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/gateway/usage":
		a.record("usage", body)
		batch := decode[struct {
			Events []usageEvent `json:"events"`
		}](a.t, body)
		a.checkUsage(body, batch.Events)
		a.events = append(a.events, batch.Events...)
		if a.usageFails > 0 {
			a.usageFails--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		results := make([]map[string]string, 0, len(batch.Events))
		for _, e := range batch.Events {
			res := map[string]string{"event_id": e.EventID, "status": "applied"}
			if a.rejectSeq[e.Seq] {
				res["status"], res["reason"] = "rejected", "invalid_period"
			}
			results = append(results, res)
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/gateway/line-events":
		a.record("line-events", body)
		batch := decode[struct {
			Events []lineEvent `json:"events"`
		}](a.t, body)
		results := make([]map[string]string, 0, len(batch.Events))
		for _, e := range batch.Events {
			a.checkLineEvent(e)
			results = append(results, map[string]string{"event_id": e.EventID, "status": "applied"})
		}
		a.lineEvents = append(a.lineEvents, batch.Events...)
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	default:
		a.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (a *fakeAPI) record(op string, body []byte) {
	a.calls = append(a.calls, apiCall{op: op, body: string(body)})
}

func (a *fakeAPI) checkLeaseRequest(r leaseRequest) {
	if !uuidV7Pattern.MatchString(r.RequestID) || !uuidV7Pattern.MatchString(r.SessionID) {
		a.t.Errorf("lease request IDs are not UUIDv7: %+v", r)
	}
	if !idPattern.MatchString(r.PeerID) || !idPattern.MatchString(r.LineID) {
		a.t.Errorf("lease request peer_id %q or line_id %q is not an ID", r.PeerID, r.LineID)
	}
}

// checkUsage applies the contract's rules for a usage batch.
func (a *fakeAPI) checkUsage(raw []byte, events []usageEvent) {
	if len(events) == 0 || len(events) > 100 {
		a.t.Errorf("usage batch has %d events; the contract allows 1 to 100", len(events))
	}
	if len(raw) > 131072 {
		a.t.Errorf("usage batch is %d bytes, over the 128 KB limit", len(raw))
	}
	for _, e := range events {
		if !uuidV7Pattern.MatchString(e.EventID) || !uuidV7Pattern.MatchString(e.SessionID) {
			a.t.Errorf("usage event IDs are not UUIDv7: %+v", e)
		}
		if !idPattern.MatchString(e.PeerID) || !idPattern.MatchString(e.LineID) || !idPattern.MatchString(e.LeaseID) {
			a.t.Errorf("usage event has a malformed peer, line or lease ID: %+v", e)
		}
		if (e.Seq == 0) != (e.SessionStart != nil) {
			a.t.Errorf("session_start must be on seq 0 and only there: %+v", e)
		}
		start, err1 := time.Parse(time.RFC3339, e.PeriodStart)
		end, err2 := time.Parse(time.RFC3339, e.PeriodEnd)
		if err1 != nil || err2 != nil || !end.After(start) {
			a.t.Errorf("usage event period %s..%s is not a valid, non-empty period", e.PeriodStart, e.PeriodEnd)
		}
		// The event's period must be visible at millisecond precision, as the API parses it.
		if end.Truncate(time.Millisecond).Sub(start.Truncate(time.Millisecond)) < time.Millisecond {
			a.t.Errorf("usage event period %s..%s is shorter than a millisecond", e.PeriodStart, e.PeriodEnd)
		}
	}
	// Unknown fields are refused by the API; a destination can never be sent.
	var generic struct {
		Events []map[string]any `json:"events"`
	}
	_ = json.Unmarshal(raw, &generic)
	allowed := map[string]bool{"event_id": true, "session_id": true, "peer_id": true, "line_id": true, "lease_id": true,
		"seq": true, "bytes_up": true, "bytes_down": true, "period_start": true, "period_end": true, "final": true, "session_start": true}
	for _, e := range generic.Events {
		for k := range e {
			if !allowed[k] {
				a.t.Errorf("usage event has field %q, which the contract does not allow", k)
			}
		}
	}
}

func (a *fakeAPI) checkLineEvent(e lineEvent) {
	if !uuidV7Pattern.MatchString(e.EventID) || !idPattern.MatchString(e.LineID) {
		a.t.Errorf("line event IDs are malformed: %+v", e)
	}
	if e.Type != "online" && e.Type != "offline" {
		a.t.Errorf("line event type %q", e.Type)
	}
	if _, err := netip.ParseAddr(e.ObservedIP); err != nil {
		a.t.Errorf("line event observed_ip %q: %v", e.ObservedIP, err)
	}
	if !semverPattern.MatchString(e.AgentVersion) {
		a.t.Errorf("line event agent_version %q is not a semantic version", e.AgentVersion)
	}
	if _, err := time.Parse(time.RFC3339, e.At); err != nil {
		a.t.Errorf("line event at %q: %v", e.At, err)
	}
}

func (a *fakeAPI) setConfig(change func(cfg map[string]any)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	change(a.config)
}

func (a *fakeAPI) setLeaseFn(fn func(leaseRequest) (int, any)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.leaseFn = fn
}

func (a *fakeAPI) lineEventsReceived() []lineEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]lineEvent(nil), a.lineEvents...)
}

func (a *fakeAPI) setClockOffset(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.offset = d
}

func (a *fakeAPI) failUsage(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usageFails = n
}

func (a *fakeAPI) rejectSeqs(seqs ...int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range seqs {
		a.rejectSeq[s] = true
	}
}

func (a *fakeAPI) ifNoneMatch() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.configTags...)
}

func (a *fakeAPI) leaseRequests() []leaseRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]leaseRequest(nil), a.leases...)
}

func (a *fakeAPI) usageEvents() []usageEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]usageEvent(nil), a.events...)
}

func (a *fakeAPI) returned() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.returns...)
}

func (a *fakeAPI) callLog() []apiCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]apiCall(nil), a.calls...)
}

func decode[T any](t *testing.T, b []byte) T {
	var v T
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Errorf("request body %s does not match the contract: %v", b, err)
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fakeGateway stands in for gateway.Server. Tests set each peer's metered
// usage by hand.
type fakeGateway struct {
	mu        sync.Mutex
	peers     map[gateway.Key]gateway.Peer
	lines     map[string]gateway.Line
	usage     map[gateway.Key]gateway.Usage
	suspended map[gateway.Key]bool
	resets    map[gateway.Key]int
	removed   []gateway.Key
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{
		peers:     map[gateway.Key]gateway.Peer{},
		lines:     map[string]gateway.Line{},
		usage:     map[gateway.Key]gateway.Usage{},
		suspended: map[gateway.Key]bool{},
		resets:    map[gateway.Key]int{},
	}
}

func (g *fakeGateway) AddPeer(p gateway.Peer) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.peers[p.PublicKey] = p
	return nil
}

func (g *fakeGateway) RemovePeer(k gateway.Key) (gateway.Usage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.usage[k]
	delete(g.peers, k)
	delete(g.usage, k)
	delete(g.suspended, k)
	g.resets[k]++
	g.removed = append(g.removed, k)
	return u, nil
}

func (g *fakeGateway) AddLine(l gateway.Line) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, other := range g.lines {
		if id != l.ID && other.AgentPin == l.AgentPin {
			return fmt.Errorf("the agent pin of line %s already serves another line", l.ID)
		}
	}
	g.lines[l.ID] = l
	return nil
}

func (g *fakeGateway) RemoveLine(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.lines, id)
}

func (g *fakeGateway) PeerUsage(addr netip.Addr) gateway.Usage {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, p := range g.peers {
		if p.IPv4 == addr || p.IPv6 == addr {
			return g.usage[k]
		}
	}
	return gateway.Usage{}
}

func (g *fakeGateway) SetPeerLine(k gateway.Key, lineID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.peers[k]; ok {
		p.LineID = lineID
		g.peers[k] = p
	}
}

func (g *fakeGateway) removedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.removed)
}

func (g *fakeGateway) ResetPeerFlows(k gateway.Key) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resets[k]++
}

func (g *fakeGateway) SuspendPeer(k gateway.Key, suspended bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.suspended[k] = suspended
	if suspended {
		g.resets[k]++
	}
}

// send adds bytes to a peer's meter, as traffic through the gateway would.
// A suspended peer's packets are dropped uncounted.
func (g *fakeGateway) send(t *testing.T, key string, up, down uint64) {
	t.Helper()
	k := mustKey(t, key)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.peers[k]; !ok {
		t.Fatalf("peer %s is not on the gateway", key)
	}
	if g.suspended[k] {
		return
	}
	u := g.usage[k]
	u.Up += up
	u.Down += down
	g.usage[k] = u
}

func (g *fakeGateway) peerUsage(t *testing.T, key string) gateway.Usage {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.usage[mustKey(t, key)]
}

func (g *fakeGateway) peer(t *testing.T, key string) (gateway.Peer, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.peers[mustKey(t, key)]
	return p, ok
}

func (g *fakeGateway) line(id string) (gateway.Line, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	l, ok := g.lines[id]
	return l, ok
}

func (g *fakeGateway) isSuspended(t *testing.T, key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.suspended[mustKey(t, key)]
}

func (g *fakeGateway) resetCount(t *testing.T, key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.resets[mustKey(t, key)]
}

func mustKey(t *testing.T, s string) gateway.Key {
	t.Helper()
	k, err := gateway.ParseKey(s)
	if err != nil {
		t.Fatalf("key %q: %v", s, err)
	}
	return k
}

func mustPin(t *testing.T, hexPin string) relayprotocol.Pin {
	t.Helper()
	b, err := hex.DecodeString(hexPin)
	if err != nil || len(b) != len(relayprotocol.Pin{}) {
		t.Fatalf("pin %q: %v", hexPin, err)
	}
	return relayprotocol.Pin(b)
}

// fixture is a controller wired to a fake API and a fake gateway, with the
// contract's example config applied and its line online.
type fixture struct {
	api   *fakeAPI
	gw    *fakeGateway
	clock *fakeClock
	ctl   *controlplane.Controller
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := newBareFixture(t)
	if err := f.ctl.SyncConfig(t.Context()); err != nil {
		t.Fatalf("sync config: %v", err)
	}
	f.ctl.OnLineEvent(gateway.LineEvent{LineID: lineID, Online: true, ObservedIP: netip.MustParseAddr("203.0.113.47"), AgentVersion: "0.1.0"})
	return f
}

// newBareFixture is a fixture before its first config sync.
func newBareFixture(t *testing.T) *fixture {
	t.Helper()
	api := newFakeAPI(t)
	gw := newFakeGateway()
	clock := newClock()
	api.clock = clock
	ctl := controlplane.New(controlplane.NewClient(api.srv.URL, testToken), controlplane.Options{
		Clock:  clock,
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})
	ctl.Attach(gw)
	t.Cleanup(ctl.Settle)
	return &fixture{api: api, gw: gw, clock: clock, ctl: ctl}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(b)))
	return len(b), nil
}

// peerA is the first peer of the example config, as the gateway knows it.
func (f *fixture) peerA(t *testing.T) gateway.Peer {
	t.Helper()
	p, ok := f.gw.peer(t, peerAKey)
	if !ok {
		t.Fatal("peer A is not on the gateway")
	}
	return p
}

// access asks the controller what a new flow of peer A may reach.
func (f *fixture) access(t *testing.T) gateway.Access {
	t.Helper()
	return f.ctl.Access(t.Context(), f.peerA(t))
}

// step advances the clock and runs the controller's timed work, then waits
// for the API calls it started.
func (f *fixture) step(t *testing.T, d time.Duration) {
	t.Helper()
	f.clock.Advance(d)
	f.ctl.Step()
	f.ctl.Settle()
}

func (f *fixture) flush(t *testing.T) {
	t.Helper()
	f.ctl.Flush(t.Context())
}
