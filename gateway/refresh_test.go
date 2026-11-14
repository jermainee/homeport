package gateway_test

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/jermainee/homeport/gateway"
	"github.com/jermainee/homeport/gateway/internal/controlplane"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

const (
	newLineID   = "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e6"
	lineOwnerID = "01a1c4af-897b-7f0a-a915-c1ba8ba7b153"
)

// runFromAPI runs a gateway from api the way gateway/service does, with
// the controller's loops, until the test ends. It returns once the gateway
// has fetched its first config.
func runFromAPI(t *testing.T, api *miniAPI) *gateway.Server {
	t.Helper()
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
		QUICListen:     "127.0.0.1:0",
		Certificate:    cert,
		Access:         ctl,
		OnLineEvent:    ctl.OnLineEvent,
		OnUnknownAgent: ctl.RefreshConfig,
	})
	ctl.Attach(gw)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctl.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		ctl.Close(context.Background())
	})
	waitUntil(t, func() bool { return len(api.configFetches()) > 0 })
	return gw
}

// apiWithoutLines is a control API whose config has no line yet.
func apiWithoutLines() *miniAPI {
	return &miniAPI{config: map[string]any{
		"etag":            `"cfg-1"`,
		"topup_hostnames": []string{"homeport.example"},
		"peers":           []map[string]string{},
		"lines":           []map[string]string{},
	}}
}

func configLine(pin relayprotocol.Pin) map[string]string {
	return map[string]string{
		"line_id":            newLineID,
		"owner_member_id":    lineOwnerID,
		"device_spki_sha256": hex.EncodeToString(pin[:]),
	}
}

// certificateRefused reports that the gateway refused the agent's
// certificate during the handshake: a TLS bad_certificate alert, which QUIC
// carries as the transport error 0x100 + 42 (RFC 9001, section 4.8).
func certificateRefused(err error) bool {
	var te *quic.TransportError
	return errors.As(err, &te) && te.Remote && te.ErrorCode == 0x100+42
}

// acceptedWithin dials the gateway as the agent every 100 ms, as an agent
// retries, and fails the test unless an attempt is accepted within limit.
// It returns how long that took.
func acceptedWithin(t *testing.T, gw *gateway.Server, cert tls.Certificate, limit time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		_, err := startAgent(t, gw, cert, answer(relayprotocol.StatusOK))
		if err == nil {
			return time.Since(start)
		}
		if time.Since(start) > limit {
			t.Fatalf("the agent was still refused after %v: %v", limit, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestANewLinesAgentIsAcceptedAtItsNextAttempt(t *testing.T) {
	api := apiWithoutLines()
	gw := runFromAPI(t, api)
	agentCert, agentPin := newAgentCert(t)

	// The member turns Share on after the gateway's last config poll: the
	// API lists the line, the gateway does not know it yet.
	api.setLines(`"cfg-2"`, configLine(agentPin))
	_, err := startAgent(t, gw, agentCert, answer(relayprotocol.StatusOK))
	if !certificateRefused(err) {
		t.Fatalf("first attempt: %v; want the certificate refused", err)
	}

	took := acceptedWithin(t, gw, agentCert, time.Second)
	if !gw.LineOnline(newLineID) {
		t.Fatal("the line is not online although its agent was accepted")
	}
	t.Logf("accepted %v after the first refusal", took)
}

func TestAnAgentWhoseLineAppearsAfterItFirstConnectsIsAcceptedWithinSeconds(t *testing.T) {
	api := apiWithoutLines()
	gw := runFromAPI(t, api)
	agentCert, agentPin := newAgentCert(t)

	fetched := len(api.configFetches())
	if _, err := startAgent(t, gw, agentCert, answer(relayprotocol.StatusOK)); !certificateRefused(err) {
		t.Fatalf("first attempt: %v; want the certificate refused", err)
	}
	// The refusal made the gateway fetch its config, before the API listed
	// the line. The next fetch an unknown key asks for is rate-limited.
	waitUntil(t, func() bool { return len(api.configFetches()) > fetched })
	api.setLines(`"cfg-2"`, configLine(agentPin))

	took := acceptedWithin(t, gw, agentCert, controlplane.ConfigRefreshInterval+time.Second)
	t.Logf("accepted %v after the API listed the line", took)
}

func TestUnknownAgentsCannotMakeTheGatewayHammerTheAPI(t *testing.T) {
	api := apiWithoutLines()
	gw := runFromAPI(t, api)
	initial := len(api.configFetches())

	// Clients with fresh keys dial as fast as they can. At this rate on
	// loopback, quic-go now and then loses a handshake until the client
	// gives up, with or without the controller; only an accepted one is
	// wrong.
	const flood = 2500 * time.Millisecond
	start := time.Now()
	var handshakes atomic.Int64
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for time.Since(start) < flood {
				cert, err := relayprotocol.GenerateSelfSigned("agent")
				if err != nil {
					t.Errorf("agent certificate: %v", err)
					return
				}
				err = dialUnknown(gw, cert)
				if err == nil {
					t.Error("the gateway accepted an agent whose key belongs to no line")
					return
				}
				if certificateRefused(err) {
					handshakes.Add(1)
				}
			}
		})
	}
	wg.Wait()
	// A refresh asked for near the end of the flood is still due.
	time.Sleep(controlplane.ConfigRefreshInterval + 500*time.Millisecond)
	window := time.Since(start)

	fetches := api.configFetches()[initial:]
	t.Logf("%d refused handshakes, %d config fetches in %v", handshakes.Load(), len(fetches), window.Round(time.Millisecond))
	if handshakes.Load() < 50 {
		t.Fatalf("only %d handshakes; not a flood", handshakes.Load())
	}
	if len(fetches) == 0 {
		t.Fatal("unknown agents never made the gateway fetch its config")
	}
	if most := int(window/controlplane.ConfigRefreshInterval) + 1; len(fetches) > most {
		t.Errorf("%d config fetches in %v; want at most %d, one per %v", len(fetches), window, most, controlplane.ConfigRefreshInterval)
	}
	for i := 1; i < len(fetches); i++ {
		// The fetches' arrival at the API jitters by a few milliseconds.
		if gap := fetches[i].Sub(fetches[i-1]); gap < controlplane.ConfigRefreshInterval-100*time.Millisecond {
			t.Fatalf("config fetches %d and %d came %v apart; want at least %v", i-1, i, gap, controlplane.ConfigRefreshInterval)
		}
	}
}

// dialUnknown dials the gateway as an agent and says hello. It returns the
// error that ended the attempt, or nil if the gateway answered the hello.
// The hello also draws the gateway's refusal again if the packet that
// carried it was lost.
func dialUnknown(gw *gateway.Server, cert tls.Certificate) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tlsConf := relayprotocol.AgentTLSConfig(cert, relayprotocol.AllowPins(gw.RelayPin()))
	conn, err := quic.DialAddr(ctx, gw.QUICAddr().String(), tlsConf, relayprotocol.AgentQUICConfig())
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseWithError(0, "") }()
	ctl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	if err := relayprotocol.WriteControl(ctl, relayprotocol.Hello{AgentVersion: "test", OS: runtime.GOOS}); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	_ = ctl.SetReadDeadline(deadline)
	_, err = relayprotocol.ReadControl(ctl)
	return err
}
