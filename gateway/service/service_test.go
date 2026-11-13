package service_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
	"github.com/jermainee/homeport/gateway/service"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// selfAPI answers PUT /v1/gateway/self with status and records the body.
// Every other call gets an empty config or an empty answer.
type selfAPI struct {
	status int

	mu   sync.Mutex
	self map[string]string
	auth string
}

func (a *selfAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	switch r.URL.Path {
	case "/v1/gateway/self":
		a.auth = r.Header.Get("Authorization")
		_ = json.Unmarshal(body, &a.self)
		w.WriteHeader(a.status)
	case "/v1/gateway/config":
		_, _ = w.Write([]byte(`{"etag":"\"cfg-1\"","topup_hostnames":["homeport.example"],"peers":[],"lines":[]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (a *selfAPI) announced() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.self
}

func newConfig(t *testing.T, apiURL string) service.Config {
	t.Helper()
	key, err := gateway.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := relayprotocol.GenerateSelfSigned("gateway")
	if err != nil {
		t.Fatal(err)
	}
	return service.Config{
		API:         apiURL,
		Token:       "hpgw_test.secret",
		PrivateKey:  key,
		WGEndpoint:  "gw.homeport.test:51820",
		QUICListen:  "127.0.0.1:0",
		Certificate: cert,
		Version:     "1.2.3",
		Logger:      slog.New(slog.DiscardHandler),
	}
}

func TestRunAnnouncesTheGatewayUntilItsContextEnds(t *testing.T) {
	api := &selfAPI{status: http.StatusNoContent}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cfg := newConfig(t, srv.URL)
	svc, err := service.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for api.announced() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the gateway did not announce itself")
		}
		time.Sleep(10 * time.Millisecond)
	}
	gw := svc.Server()
	pin := gw.RelayPin()
	_, quicPort, _ := net.SplitHostPort(gw.QUICAddr().String())
	want := map[string]string{
		"wg_public_key":    cfg.PrivateKey.PublicKey().String(),
		"wg_endpoint":      "gw.homeport.test:51820",
		"quic_endpoint":    "gw.homeport.test:" + quicPort,
		"quic_server_name": "gw.homeport.test",
		"quic_spki_sha256": hex.EncodeToString(pin[:]),
		"version":          "1.2.3",
	}
	got := api.announced()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("announced %s = %q, want %q", k, got[k], v)
		}
	}
	if api.auth != "Bearer hpgw_test.secret" {
		t.Errorf("Authorization = %q", api.auth)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

func TestRunStopsWhenTheAPIRefusesTheToken(t *testing.T) {
	srv := httptest.NewServer(&selfAPI{status: http.StatusUnauthorized})
	t.Cleanup(srv.Close)
	svc, err := service.New(newConfig(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- svc.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil after a refused token")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept retrying a refused token")
	}
}

func TestNewNeedsAWireGuardEndpointWithAPort(t *testing.T) {
	cfg := newConfig(t, "http://127.0.0.1:1")
	cfg.WGEndpoint = "gw.homeport.test"
	if _, err := service.New(cfg); err == nil {
		t.Fatal("New accepted a WireGuard endpoint without a port")
	}
}
