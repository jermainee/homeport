package fakeapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jermainee/homeport/e2e/internal/fakeapi"
	"github.com/jermainee/homeport/e2e/internal/memberapi"
)

const token = "hpgw_e2e.secret" // #nosec G101 -- the fake API's test token

// example reads a JSON example from contracts/examples/gateway.
func example(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "examples", "gateway", name)) // #nosec G304 -- the contract's examples
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

// sameShape reports how got differs from want in shape: the same object
// keys, the same JSON types, and array elements shaped like want's first.
func sameShape(path string, got, want any) error {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: got %T, want an object", path, got)
		}
		if gk, wk := keys(g), keys(w); !reflect.DeepEqual(gk, wk) {
			return fmt.Errorf("%s: keys %v, want %v", path, gk, wk)
		}
		for k := range w {
			if err := sameShape(path+"."+k, g[k], w[k]); err != nil {
				return err
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Errorf("%s: got %T, want an array", path, got)
		}
		for i := range g {
			if len(w) > 0 {
				if err := sameShape(fmt.Sprintf("%s[%d]", path, i), g[i], w[0]); err != nil {
					return err
				}
			}
		}
	default:
		if reflect.TypeOf(got) != reflect.TypeOf(want) {
			return fmt.Errorf("%s: got %T, want %T", path, got, want)
		}
	}
	return nil
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// newAPI serves the contract's example config.
func newAPI(t *testing.T) (*fakeapi.API, *httptest.Server) {
	t.Helper()
	var cfg fakeapi.Config
	if err := json.Unmarshal(example(t, "config.json"), &cfg); err != nil {
		t.Fatal(err)
	}
	api := fakeapi.New(token, cfg)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return api, srv
}

func call(t *testing.T, srv *httptest.Server, method, path string, body []byte, header ...string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func assertShape(t *testing.T, name string, got []byte) {
	t.Helper()
	if err := sameShape(name, decode(t, got), decode(t, example(t, name))); err != nil {
		t.Errorf("answer differs from the contract example: %v\nanswer: %s", err, got)
	}
}

func assertNoViolations(t *testing.T, api *fakeapi.API) {
	t.Helper()
	if v := api.Violations(); len(v) > 0 {
		t.Errorf("contract violations: %s", strings.Join(v, "; "))
	}
}

func TestConfigIsTheContractExample(t *testing.T) {
	api, srv := newAPI(t)
	status, h, body := call(t, srv, http.MethodGet, "/v1/gateway/config", nil)
	if status != http.StatusOK {
		t.Fatalf("config answered %d", status)
	}
	if !reflect.DeepEqual(decode(t, body), decode(t, example(t, "config.json"))) {
		t.Errorf("config = %s, want the example", body)
	}
	etag := decode(t, body).(map[string]any)["etag"].(string)
	if h.Get("ETag") != etag {
		t.Errorf("ETag header %q, body etag %q", h.Get("ETag"), etag)
	}
	if status, _, _ := call(t, srv, http.MethodGet, "/v1/gateway/config", nil, "If-None-Match", "W/"+etag); status != http.StatusNotModified {
		t.Errorf("weak If-None-Match answered %d, want 304", status)
	}
	assertNoViolations(t, api)
}

func TestAChangedConfigIsServedToAGatewayWithTheOldETag(t *testing.T) {
	api, srv := newAPI(t)
	_, _, body := call(t, srv, http.MethodGet, "/v1/gateway/config", nil)
	old := decode(t, body).(map[string]any)["etag"].(string)

	// A member turns Share on: the config lists no line before, one after.
	api.SetConfig(fakeapi.Config{ETag: `"cfg-2"`, TopUpHostnames: []string{"homeport.example"}, Peers: []fakeapi.Peer{}, Lines: []fakeapi.Line{{
		LineID:           "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e6",
		OwnerMemberID:    "01a1c4af-897b-7f0a-a915-c1ba8ba7b153",
		DeviceSPKISHA256: strings.Repeat("ab", 32),
	}}})
	status, h, body := call(t, srv, http.MethodGet, "/v1/gateway/config", nil, "If-None-Match", old)
	if status != http.StatusOK || h.Get("ETag") != `"cfg-2"` {
		t.Fatalf("config with the old etag answered %d, ETag %q; want 200 with the new config", status, h.Get("ETag"))
	}
	var got fakeapi.Config
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 1 || got.Lines[0].DeviceSPKISHA256 != strings.Repeat("ab", 32) {
		t.Errorf("lines %+v; want the new line", got.Lines)
	}
	assertNoViolations(t, api)
}

func TestExampleRequestsAreAnsweredLikeTheExamples(t *testing.T) {
	api, srv := newAPI(t)

	if status, _, body := call(t, srv, http.MethodPut, "/v1/gateway/self", example(t, "self.json")); status != http.StatusNoContent {
		t.Fatalf("self answered %d %s", status, body)
	}
	if self, ok := api.Self(); !ok || !reflect.DeepEqual(decode(t, mustJSON(t, self)), decode(t, example(t, "self.json"))) {
		t.Errorf("recorded self = %+v", self)
	}

	// The example renews a lease this API never granted, so ask for a
	// first lease with the same peer and session.
	first := decode(t, example(t, "lease-request.json")).(map[string]any)
	delete(first, "supersedes_lease_id")
	status, h, body := call(t, srv, http.MethodPost, "/v1/gateway/leases", mustJSON(t, first))
	if status != http.StatusCreated {
		t.Fatalf("lease answered %d %s", status, body)
	}
	assertShape(t, "lease-granted.json", body)
	if h.Get("Date") == "" {
		t.Error("lease answer has no Date header")
	}
	leaseID := decode(t, body).(map[string]any)["lease_id"].(string)

	// The usage example with every event moved onto the granted lease.
	batch := decode(t, example(t, "usage-batch.json")).(map[string]any)
	for _, e := range batch["events"].([]any) {
		e.(map[string]any)["lease_id"] = leaseID
	}
	events := batch["events"].([]any)
	batch["events"] = events[:2]               // the third is another peer's session
	events[1].(map[string]any)["final"] = true // so the lease may be returned
	status, _, body = call(t, srv, http.MethodPost, "/v1/gateway/usage", mustJSON(t, batch))
	if status != http.StatusOK {
		t.Fatalf("usage answered %d %s", status, body)
	}
	results := decode(t, body).(map[string]any)["results"].([]any)
	for i, r := range results {
		if r.(map[string]any)["status"] != "applied" {
			t.Errorf("event %d: %v, want applied", i, r)
		}
	}
	assertShape(t, "usage-result.json", body)

	status, _, body = call(t, srv, http.MethodPost, "/v1/gateway/line-events", example(t, "line-events.json"))
	if status != http.StatusOK || !reflect.DeepEqual(decode(t, body), decode(t, example(t, "line-events-result.json"))) {
		t.Errorf("line events answered %d %s, want the example", status, body)
	}

	status, _, body = call(t, srv, http.MethodPost, "/v1/gateway/leases/"+leaseID+"/return", example(t, "lease-return-request.json"))
	if status != http.StatusOK {
		t.Fatalf("return answered %d %s", status, body)
	}
	assertShape(t, "lease-returned.json", body)
	assertNoViolations(t, api)
}

func TestDeniedLeasesAreAnsweredLikeTheExamples(t *testing.T) {
	for _, tc := range []struct {
		answer  fakeapi.LeaseAnswer
		status  int
		example string
	}{
		{fakeapi.Deny(300), http.StatusPaymentRequired, "lease-denied.json"},
		{fakeapi.Inactive(), http.StatusForbidden, "lease-peer-inactive.json"},
	} {
		t.Run(tc.example, func(t *testing.T) {
			api, srv := newAPI(t)
			api.SetLeases(func(int, fakeapi.LeaseRequest) fakeapi.LeaseAnswer { return tc.answer })
			req := decode(t, example(t, "lease-request.json")).(map[string]any)
			delete(req, "supersedes_lease_id")
			status, _, body := call(t, srv, http.MethodPost, "/v1/gateway/leases", mustJSON(t, req))
			if status != tc.status {
				t.Fatalf("lease answered %d, want %d", status, tc.status)
			}
			if !reflect.DeepEqual(decode(t, body), decode(t, example(t, tc.example))) {
				t.Errorf("answer %s, want the example", body)
			}
			assertNoViolations(t, api)
		})
	}
}

func TestRequestsOutsideTheContractAreViolations(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":   `{"wg_public_key":"YND9Jx2S347AP/Ywks3VrMciTOYivx8pQxgWoPy1HHY=","wg_endpoint":"gw.test:51820","quic_endpoint":"gw.test:443","quic_server_name":"gw.test","quic_spki_sha256":"2ca04a183e495439c9a77a4ce5f11babba2758321eb6db8d31e85b3980aea90a","version":"0.1.0","extra":1}`,
		"missing field":   `{"wg_public_key":"YND9Jx2S347AP/Ywks3VrMciTOYivx8pQxgWoPy1HHY=","wg_endpoint":"gw.test:51820","quic_endpoint":"gw.test:443","quic_server_name":"gw.test","version":"0.1.0"}`,
		"uppercase pin":   `{"wg_public_key":"YND9Jx2S347AP/Ywks3VrMciTOYivx8pQxgWoPy1HHY=","wg_endpoint":"gw.test:51820","quic_endpoint":"gw.test:443","quic_server_name":"gw.test","quic_spki_sha256":"2CA04A183E495439C9A77A4CE5F11BABBA2758321EB6DB8D31E85B3980AEA90A","version":"0.1.0"}`,
		"invalid version": `{"wg_public_key":"YND9Jx2S347AP/Ywks3VrMciTOYivx8pQxgWoPy1HHY=","wg_endpoint":"gw.test:51820","quic_endpoint":"gw.test:443","quic_server_name":"gw.test","quic_spki_sha256":"2ca04a183e495439c9a77a4ce5f11babba2758321eb6db8d31e85b3980aea90a","version":"dev"}`,
	} {
		t.Run(name, func(t *testing.T) {
			api, srv := newAPI(t)
			if status, _, _ := call(t, srv, http.MethodPut, "/v1/gateway/self", []byte(body)); status != http.StatusBadRequest {
				t.Errorf("answered %d, want 400", status)
			}
			if len(api.Violations()) == 0 {
				t.Error("no violation recorded")
			}
		})
	}
}

func TestUsageWithoutSessionStartOnTheFirstEventIsAViolation(t *testing.T) {
	api, srv := newAPI(t)
	batch := decode(t, example(t, "usage-batch.json")).(map[string]any)
	first := batch["events"].([]any)[0].(map[string]any)
	delete(first, "session_start")
	batch["events"] = []any{first}
	if status, _, _ := call(t, srv, http.MethodPost, "/v1/gateway/usage", mustJSON(t, batch)); status != http.StatusBadRequest {
		t.Errorf("answered %d, want 400", status)
	}
	if len(api.Violations()) == 0 {
		t.Error("no violation recorded")
	}
}

func TestARetriedUsageEventIsADuplicate(t *testing.T) {
	api, srv := newAPI(t)
	req := decode(t, example(t, "lease-request.json")).(map[string]any)
	delete(req, "supersedes_lease_id")
	_, _, body := call(t, srv, http.MethodPost, "/v1/gateway/leases", mustJSON(t, req))
	leaseID := decode(t, body).(map[string]any)["lease_id"].(string)
	batch := decode(t, example(t, "usage-batch.json")).(map[string]any)
	ev := batch["events"].([]any)[0].(map[string]any)
	ev["lease_id"] = leaseID
	batch["events"] = []any{ev}

	api.FailNext(fakeapi.PathUsage, fakeapi.FailWith500)
	if status, _, _ := call(t, srv, http.MethodPost, "/v1/gateway/usage", mustJSON(t, batch)); status != http.StatusInternalServerError {
		t.Fatalf("first try answered %d, want 500", status)
	}
	if n := len(api.Usage()); n != 0 {
		t.Fatalf("%d events applied after a 500", n)
	}
	if calls := api.Calls(); len(calls) == 0 || !reflect.DeepEqual(calls[len(calls)-1].EventIDs, []string{ev["event_id"].(string)}) {
		t.Errorf("the failed call did not record its event")
	}
	for i, want := range []string{"applied", "duplicate"} {
		_, _, body := call(t, srv, http.MethodPost, "/v1/gateway/usage", mustJSON(t, batch))
		if got := decode(t, body).(map[string]any)["results"].([]any)[0].(map[string]any)["status"]; got != want {
			t.Errorf("try %d: %v, want %s", i+2, got, want)
		}
	}
	if n := len(api.Usage()); n != 1 {
		t.Errorf("%d events applied, want 1", n)
	}
	assertNoViolations(t, api)
}

// The example config's peer A and its fallback line.
const (
	examplePeerA    = "01a1c4af-89c1-7c5b-a49a-3d557fc869e7"
	examplePrimary  = "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e6"
	exampleFallback = "01a1c4af-8c3e-7d21-9b4a-6f0e2d9c1a57"
)

// leaseOn asks for a first lease for peer A on lineID and returns the
// answer's status and lease ID.
func leaseOn(t *testing.T, srv *httptest.Server, sessionID, lineID string) (int, string) {
	t.Helper()
	req := map[string]any{"request_id": memberapi.NewRequestID(), "peer_id": examplePeerA, "session_id": sessionID}
	if lineID != "" {
		req["line_id"] = lineID
	}
	status, _, body := call(t, srv, http.MethodPost, "/v1/gateway/leases", mustJSON(t, req))
	if status != http.StatusCreated {
		return status, ""
	}
	return status, decode(t, body).(map[string]any)["lease_id"].(string)
}

// eventOn is a first usage event of sessionID on lineID against leaseID.
func eventOn(t *testing.T, sessionID, lineID, leaseID string) []byte {
	t.Helper()
	batch := decode(t, example(t, "usage-batch.json")).(map[string]any)
	ev := batch["events"].([]any)[0].(map[string]any)
	ev["event_id"], ev["session_id"], ev["peer_id"] = memberapi.NewRequestID(), sessionID, examplePeerA
	ev["line_id"], ev["lease_id"] = lineID, leaseID
	batch["events"] = []any{ev}
	return mustJSON(t, batch)
}

func TestALeaseOnAFallbackLineCountsUsageOnThatLine(t *testing.T) {
	api, srv := newAPI(t)
	session := memberapi.NewRequestID()
	status, leaseID := leaseOn(t, srv, session, exampleFallback)
	if status != http.StatusCreated {
		t.Fatalf("lease on the fallback line answered %d, want 201", status)
	}

	_, _, body := call(t, srv, http.MethodPost, "/v1/gateway/usage", eventOn(t, session, exampleFallback, leaseID))

	if got := decode(t, body).(map[string]any)["results"].([]any)[0].(map[string]any)["status"]; got != "applied" {
		t.Errorf("usage on the lease's line: %v, want applied", got)
	}
	if reqs := api.LeaseRequests(); len(reqs) != 1 || reqs[0].LineID != exampleFallback {
		t.Errorf("lease requests %+v, want one on the fallback line", reqs)
	}
	assertNoViolations(t, api)
}

func TestUsageOnAnotherLineThanItsLeaseIsAViolation(t *testing.T) {
	api, srv := newAPI(t)
	session := memberapi.NewRequestID()
	_, leaseID := leaseOn(t, srv, session, exampleFallback)

	_, _, body := call(t, srv, http.MethodPost, "/v1/gateway/usage", eventOn(t, session, examplePrimary, leaseID))

	result := decode(t, body).(map[string]any)["results"].([]any)[0].(map[string]any)
	if result["status"] != "rejected" || result["reason"] != "lease_mismatch" {
		t.Errorf("usage on another line: %v, want rejected lease_mismatch", result)
	}
	if len(api.Violations()) == 0 {
		t.Error("no violation recorded")
	}
}

func TestALeaseOnALineThatIsNotThePeersIsRefused(t *testing.T) {
	api, srv := newAPI(t)

	status, _ := leaseOn(t, srv, memberapi.NewRequestID(), "01a1c4af-0000-7000-8000-00000000abcd")

	if status != http.StatusForbidden {
		t.Errorf("lease on a line that is not the peer's answered %d, want 403", status)
	}
	assertNoViolations(t, api)
}

func TestARenewalOnAnotherLineIsAViolation(t *testing.T) {
	api, srv := newAPI(t)
	session := memberapi.NewRequestID()
	_, leaseID := leaseOn(t, srv, session, exampleFallback)
	req := map[string]any{"request_id": memberapi.NewRequestID(), "peer_id": examplePeerA, "session_id": session, "line_id": examplePrimary, "supersedes_lease_id": leaseID}

	if status, _, _ := call(t, srv, http.MethodPost, "/v1/gateway/leases", mustJSON(t, req)); status != http.StatusBadRequest {
		t.Errorf("renewal on another line answered %d, want 400", status)
	}
	if len(api.Violations()) == 0 {
		t.Error("no violation recorded")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
