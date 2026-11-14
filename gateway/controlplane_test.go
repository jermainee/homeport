package gateway_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
	"github.com/jermainee/homeport/gateway/internal/controlplane"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// miniAPI is just enough of the control API for an end-to-end run.
type miniAPI struct {
	mu         sync.Mutex
	config     map[string]any
	fetches    []time.Time // when each config request arrived
	usage      []map[string]any
	lineEvents []map[string]any
	returned   []string
}

// configFetches returns when each config request arrived, oldest first.
func (a *miniAPI) configFetches() []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Time(nil), a.fetches...)
}

// setLines replaces the config's lines and gives it a new etag.
func (a *miniAPI) setLines(etag string, lines ...map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config["etag"] = etag
	a.config["lines"] = append([]map[string]string{}, lines...)
}

// setPeers replaces the config's peers and gives it a new etag.
func (a *miniAPI) setPeers(etag string, peers ...map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config["etag"] = etag
	a.config["peers"] = append([]map[string]string{}, peers...)
}

func (a *miniAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	a.mu.Lock()
	defer a.mu.Unlock()
	reply := func(status int, v any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	results := func(events []any) map[string]any {
		var res []map[string]string
		for _, e := range events {
			res = append(res, map[string]string{"event_id": e.(map[string]any)["event_id"].(string), "status": "applied"})
		}
		return map[string]any{"results": res}
	}
	switch {
	case r.URL.Path == "/v1/gateway/config":
		a.fetches = append(a.fetches, time.Now())
		if r.Header.Get("If-None-Match") == a.config["etag"] {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		reply(http.StatusOK, a.config)
	case r.URL.Path == "/v1/gateway/leases":
		lease := contractExample("lease-granted.json")
		lease["expires_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		reply(http.StatusCreated, lease)
	case strings.HasSuffix(r.URL.Path, "/return"):
		a.returned = append(a.returned, r.URL.Path)
		reply(http.StatusOK, contractExample("lease-returned.json"))
	case r.URL.Path == "/v1/gateway/usage":
		for _, e := range req["events"].([]any) {
			a.usage = append(a.usage, e.(map[string]any))
		}
		reply(http.StatusOK, results(req["events"].([]any)))
	case r.URL.Path == "/v1/gateway/line-events":
		for _, e := range req["events"].([]any) {
			a.lineEvents = append(a.lineEvents, e.(map[string]any))
		}
		reply(http.StatusOK, results(req["events"].([]any)))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func contractExample(name string) map[string]any {
	b, err := os.ReadFile(filepath.Join("..", "contracts", "examples", "gateway", name)) // #nosec G304 -- the contract's examples
	if err != nil {
		panic(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		panic(err)
	}
	return v
}

func TestGatewayRunsFromTheControlAPI(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(randomBytes(300_000))
	}))
	t.Cleanup(origin.Close)
	clientKey := newKey(t)
	agentCert, agentPin := newAgentCert(t)
	const lineID = "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e6"
	api := &miniAPI{config: map[string]any{
		"etag":            `"cfg-1"`,
		"topup_hostnames": []string{"homeport.example"},
		"peers": []map[string]string{{
			"peer_id":    "01a1c4af-89c1-7c5b-a49a-3d557fc869e7",
			"member_id":  "01a1c4af-8795-7a28-9249-adbd2ba1c04a",
			"public_key": clientKey.PublicKey().String(),
			"ipv4":       "10.77.0.2",
			"ipv6":       "fd77:6870::2",
			"line_id":    lineID,
		}},
		"lines": []map[string]string{{
			"line_id":            lineID,
			"owner_member_id":    "01a1c4af-897b-7f0a-a915-c1ba8ba7b153",
			"device_spki_sha256": hex.EncodeToString(agentPin[:]),
		}},
	}}
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)

	ctl := controlplane.New(controlplane.NewClient(apiSrv.URL, "hpgw_test.secret"), controlplane.Options{
		Logger: slog.New(slog.DiscardHandler),
	})
	cert, err := relayprotocol.GenerateSelfSigned("gateway")
	if err != nil {
		t.Fatalf("gateway certificate: %v", err)
	}
	gw := newGatewayWith(t, gateway.Config{
		QUICListen:  "127.0.0.1:0",
		Certificate: cert,
		Access:      ctl,
		OnLineEvent: ctl.OnLineEvent,
	})
	ctl.Attach(gw)
	ctx := t.Context()
	if err := ctl.SyncConfig(ctx); err != nil {
		t.Fatalf("sync config: %v", err)
	}
	if _, err := startAgent(t, gw, agentCert, relayTo(origin.Listener.Addr().String())); err != nil {
		t.Fatalf("agent: %v", err)
	}
	client := newClient(t, gw, clientKey, netip.MustParseAddr("10.77.0.2"))

	if got := client.get(t, "http://11.22.33.44/"); len(got) != 300_000 {
		t.Fatalf("got %d bytes through the gateway", len(got))
	}
	final := waitStable(t, 300*time.Millisecond, func() gateway.Usage { return gw.PeerUsage(netip.MustParseAddr("10.77.0.2")) })
	ctl.Close(ctx) // ends the session: final event, then the lease return

	api.mu.Lock()
	var reported gateway.Usage
	for _, e := range api.usage {
		reported.Up += uint64(e["bytes_up"].(float64))
		reported.Down += uint64(e["bytes_down"].(float64))
	}
	returned := len(api.returned)
	api.mu.Unlock()
	if reported != final {
		t.Errorf("usage events report %+v, the gateway metered %+v", reported, final)
	}
	if returned != 1 {
		t.Errorf("%d lease returns, want 1", returned)
	}

	// Dropping the line disconnects its agent while the controller applies
	// the config; the offline event must not deadlock.
	api.mu.Lock()
	api.config["etag"] = `"cfg-2"`
	api.config["lines"] = []map[string]string{}
	api.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- ctl.SyncConfig(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sync config: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("applying a config without the line hung")
	}
	ctl.Flush(context.Background())
	api.mu.Lock()
	defer api.mu.Unlock()
	if n := len(api.lineEvents); n != 2 || api.lineEvents[0]["type"] != "online" || api.lineEvents[1]["type"] != "offline" {
		t.Errorf("line events %v; want online, then offline", api.lineEvents)
	}
}
