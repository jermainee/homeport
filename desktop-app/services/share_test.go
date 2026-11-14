package services_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jermainee/homeport/agent"
	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/jermainee/homeport/desktop-app/internal/apiclient"
	"github.com/jermainee/homeport/desktop-app/internal/fakeapi"
	"github.com/jermainee/homeport/desktop-app/internal/profile"
	"github.com/jermainee/homeport/desktop-app/services"
)

// fakeAgent stands in for the sharing agent. Run blocks until its context
// ends; the test sets what Stats reports.
type fakeAgent struct {
	cfg     agent.Config
	started chan struct{}
	stopped chan struct{}

	mu    sync.Mutex
	stats agent.Stats
}

func (a *fakeAgent) Run(ctx context.Context) error {
	close(a.started)
	<-ctx.Done()
	a.set(func(s *agent.Stats) { s.State = agent.StateStopped })
	close(a.stopped)
	return nil
}

func (a *fakeAgent) Stats() agent.Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}

func (a *fakeAgent) set(f func(*agent.Stats)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(&a.stats)
}

// agents makes fake agents and keeps every one it made.
type agents struct {
	mu   sync.Mutex
	made []*fakeAgent
}

func (as *agents) New(cfg agent.Config) (services.Agent, error) {
	a := &fakeAgent{cfg: cfg, started: make(chan struct{}), stopped: make(chan struct{})}
	a.stats.State = agent.StateConnecting
	as.mu.Lock()
	defer as.mu.Unlock()
	as.made = append(as.made, a)
	return a, nil
}

// running waits for the latest agent to run and returns it.
func (as *agents) running(t *testing.T) *fakeAgent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		as.mu.Lock()
		var a *fakeAgent
		if n := len(as.made); n > 0 {
			a = as.made[n-1]
		}
		as.mu.Unlock()
		if a != nil {
			select {
			case <-a.started:
				return a
			case <-time.After(5 * time.Millisecond):
			}
			continue
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no agent is running")
	return nil
}

// nth waits for the nth agent (from 1) to run and returns it.
func (as *agents) nth(t *testing.T, n int) *fakeAgent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		as.mu.Lock()
		var a *fakeAgent
		if len(as.made) >= n {
			a = as.made[n-1]
		}
		as.mu.Unlock()
		if a != nil {
			<-a.started
			return a
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("agent %d never ran", n)
	return nil
}

// eventually fails the test unless cond holds within a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (as *agents) count() int {
	as.mu.Lock()
	defer as.mu.Unlock()
	return len(as.made)
}

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// shareFixture is a signed-in member with a ShareService on their profile.
type shareFixture struct {
	api    *fakeapi.Server
	dir    string
	agents *agents
	clock  *clock
	share  *services.ShareService
}

// The gateway's certificate key in these tests is the Ed25519 key with an
// all-zero seed. Its pin, from `openssl pkey -pubout -outform DER` and
// SHA-256, in the contract's hex and in relay-protocol's base64:
const (
	gatewayPinHex    = "339e2ff917630507b6a423b5ce084e285d1fa65d93b3e27a6195d3bbebc9ae23"
	gatewayPinBase64 = "M54v+RdjBQe2pCO1zghOKF0fpl2Ts+J6YZXTu+vJriM="
)

// platforms maps GOOS to the contract's platform names.
var platforms = map[string]string{"darwin": "macos", "linux": "linux", "windows": "windows"}

func newShare(t *testing.T) *shareFixture {
	t.Helper()
	api := fakeapi.New(t)
	dir := t.TempDir()
	signIn(t, api, newAuth(t, api, dir), email)
	f := &shareFixture{api: api, dir: dir, agents: &agents{}, clock: &clock{t: time.Date(2026, 11, 13, 21, 0, 0, 0, time.Local)}}
	f.share = f.open(t)
	return f
}

// verified is newShare for a member who passed the identity check, with a
// gateway that has announced itself.
func verified(t *testing.T) *shareFixture {
	t.Helper()
	f := newShare(t)
	f.api.Verify(t, email)
	f.api.SetGateway(fakeapi.Gateway{
		QUICEndpoint: "fra1.gw.homeport.test:443",
		ServerName:   "fra1.gw.homeport.test",
		SPKISHA256:   gatewayPinHex,
	})
	return f
}

// open opens the profile again, as the app does when it starts.
func (f *shareFixture) open(t *testing.T) *services.ShareService {
	t.Helper()
	p, err := profile.Open(f.dir)
	if err != nil {
		t.Fatalf("open profile: %v", err)
	}
	share := services.NewShareService(apiclient.New(f.api.URL, f.api.Client()), p, services.ShareOptions{
		NewAgent:   f.agents.New,
		DeviceName: "Living room Mac mini",
		RetryMin:   10 * time.Millisecond,
		RetryMax:   20 * time.Millisecond,
		Now:        f.clock.Now,
		// How often a running Share publishes the agent's state.
		StatusInterval: 5 * time.Millisecond,
	})
	// The app quits before the test's temporary profile goes.
	t.Cleanup(func() { _ = share.ServiceShutdown() })
	return share
}

func TestShareNeedsAVerifiedIdentity(t *testing.T) {
	f := newShare(t)

	got := f.share.SetSharing(t.Context(), true)

	if got.State != services.ShareError || got.Error != services.ErrIdentityRequired {
		t.Errorf("status = %+v, want state %q with error %q", got, services.ShareError, services.ErrIdentityRequired)
	}
	if got.Sharing {
		t.Error("the switch is on, want it off")
	}
	if d := f.api.Devices(t, email); len(d) != 0 {
		t.Errorf("an unverified member registered devices: %+v", d)
	}
}

func TestShareRegistersThisMacWithItsAgentKey(t *testing.T) {
	f := verified(t)

	f.share.SetSharing(t.Context(), true)

	a := f.agents.running(t)
	devices := f.api.Devices(t, email)
	if len(devices) != 1 {
		t.Fatalf("member has %d devices, want 1", len(devices))
	}
	d := devices[0]
	if want := platforms[runtime.GOOS]; d.Name != "Living room Mac mini" || d.Platform != want {
		t.Errorf("device = %q on %q, want the computer's name on %q", d.Name, d.Platform, want)
	}
	cert, err := x509.ParseCertificate(a.cfg.Certificate.Certificate[0])
	if err != nil {
		t.Fatalf("agent certificate: %v", err)
	}
	pub := cert.PublicKey.(ed25519.PublicKey)
	if got := base64.StdEncoding.EncodeToString(pub); got != d.AgentPublicKey {
		t.Errorf("agent presents key %s, but the device registered %s", got, d.AgentPublicKey)
	}
}

func TestShareCreatesTheLineAndDialsItsGateway(t *testing.T) {
	f := verified(t)

	got := f.share.SetSharing(t.Context(), true)

	if got.State != services.ShareConnecting || !got.Sharing {
		t.Errorf("status = %+v, want connecting with the switch on", got)
	}
	lines := f.api.Lines(t, email)
	if len(lines) != 1 || !lines[0].SharingEnabled {
		t.Fatalf("lines = %+v, want one line with sharing on", lines)
	}
	a := f.agents.running(t)
	if a.cfg.GatewayAddr != "fra1.gw.homeport.test:443" {
		t.Errorf("agent dials %q, want the line's gateway", a.cfg.GatewayAddr)
	}
	// The contract sends the pin as hex; the agent pins the same 32 bytes,
	// whose text form is base64.
	if got := a.cfg.GatewayPin.String(); got != gatewayPinBase64 {
		t.Errorf("agent pins %s, want %s", got, gatewayPinBase64)
	}
}

func TestShareRefusesAGatewayPinThatIsNotHex(t *testing.T) {
	f := verified(t)
	f.api.SetGateway(fakeapi.Gateway{
		QUICEndpoint: "fra1.gw.homeport.test:443",
		ServerName:   "fra1.gw.homeport.test",
		SPKISHA256:   gatewayPinBase64, // base64, not the contract's hex
	})

	got := f.share.SetSharing(t.Context(), true)

	if got.State != services.ShareError || got.Error != services.ErrUnexpected {
		t.Errorf("status = %+v, want error %q", got, services.ErrUnexpected)
	}
	if n := f.agents.count(); n != 0 {
		t.Errorf("%d agents started without a valid pin", n)
	}
}

func TestTurningShareOffStopsTheAgentAndTellsTheAPI(t *testing.T) {
	f := verified(t)
	f.share.SetSharing(t.Context(), true)
	a := f.agents.running(t)

	got := f.share.SetSharing(t.Context(), false)

	if got.State != services.ShareOff || got.Sharing {
		t.Errorf("status = %+v, want off", got)
	}
	select {
	case <-a.stopped:
	default:
		t.Error("the agent still runs")
	}
	if lines := f.api.Lines(t, email); lines[0].SharingEnabled {
		t.Error("the API still has sharing on")
	}
}

func TestShareKeepsTheSameAgentKeyAcrossRestarts(t *testing.T) {
	f := verified(t)
	f.share.SetSharing(t.Context(), true)
	first := f.agents.running(t)
	f.share.SetSharing(t.Context(), false)

	// The app starts again with the same profile.
	f.share = f.open(t)
	f.share.SetSharing(t.Context(), true)
	second := f.agents.running(t)

	if first == second {
		t.Fatal("no new agent started")
	}
	if !first.cfg.Certificate.PrivateKey.(ed25519.PrivateKey).Equal(second.cfg.Certificate.PrivateKey) {
		t.Error("the agent has a new key after a restart")
	}
	if d := f.api.Devices(t, email); len(d) != 1 {
		t.Errorf("member has %d devices, want 1", len(d))
	}
	if l := f.api.Lines(t, email); len(l) != 1 || !l[0].SharingEnabled {
		t.Errorf("lines = %+v, want the same line, sharing", l)
	}
}

func TestShareFollowsTheAgent(t *testing.T) {
	cases := []struct {
		name  string
		stats agent.Stats
		want  services.ShareState
		err   services.ErrorCode
	}{
		{"dialing the gateway", agent.Stats{State: agent.StateConnecting}, services.ShareConnecting, ""},
		{"accepted by the gateway", agent.Stats{State: agent.StateConnected, FlowsActive: 3}, services.ShareOnline, ""},
		{"another VPN holds the default route", agent.Stats{State: agent.StatePaused, OtherVPN: true}, services.SharePaused, ""},
		{"gateway unreachable", agent.Stats{State: agent.StateConnecting, LastError: errors.New("timeout")}, services.ShareOffline, services.ErrOffline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := verified(t)
			f.share.SetSharing(t.Context(), true)
			a := f.agents.running(t)

			a.set(func(s *agent.Stats) { *s = tc.stats })
			got := f.share.Status()

			if got.State != tc.want || got.Error != tc.err || !got.Sharing {
				t.Errorf("status = %+v, want %q with error %q", got, tc.want, tc.err)
			}
			if got.FlowsActive != tc.stats.FlowsActive {
				t.Errorf("flows = %d, want %d", got.FlowsActive, tc.stats.FlowsActive)
			}
		})
	}
}

func TestShareResumesWhenTheOtherVPNIsGone(t *testing.T) {
	f := verified(t)
	f.share.SetSharing(t.Context(), true)
	a := f.agents.running(t)
	a.set(func(s *agent.Stats) { s.State, s.OtherVPN = agent.StatePaused, true })
	if got := f.share.Status(); got.State != services.SharePaused {
		t.Fatalf("status = %+v, want paused", got)
	}

	a.set(func(s *agent.Stats) { s.State, s.OtherVPN = agent.StateConnected, false })

	if got := f.share.Status(); got.State != services.ShareOnline {
		t.Errorf("status = %+v, want online", got)
	}
	if n := f.agents.count(); n != 1 {
		t.Errorf("%d agents, want the same one to carry on", n)
	}
}

func TestShareKeepsTryingWhileTheAPICantBeReached(t *testing.T) {
	f := verified(t)
	f.api.SetOffline(true)

	got := f.share.SetSharing(t.Context(), true)

	if got.State != services.ShareOffline || got.Error != services.ErrOffline || !got.Sharing {
		t.Errorf("status = %+v, want offline with the switch on", got)
	}
	f.api.SetOffline(false)
	f.agents.running(t)
	if got := f.share.Status(); got.State != services.ShareConnecting || got.Error != "" {
		t.Errorf("status once the API answers = %+v, want connecting", got)
	}
}

func TestShareWaitsForAGateway(t *testing.T) {
	f := newShare(t)
	f.api.Verify(t, email)

	got := f.share.SetSharing(t.Context(), true)

	if got.State != services.ShareOffline || got.Error != services.ErrGatewayUnavailable {
		t.Errorf("status = %+v, want offline with error %q", got, services.ErrGatewayUnavailable)
	}
	f.api.SetGateway(fakeapi.Gateway{QUICEndpoint: "fra1.gw.homeport.test:443", ServerName: "fra1.gw.homeport.test", SPKISHA256: gatewayPinHex})
	if a := f.agents.running(t); a.cfg.GatewayAddr != "fra1.gw.homeport.test:443" {
		t.Errorf("agent dials %q", a.cfg.GatewayAddr)
	}
	if d := f.api.Devices(t, email); len(d) != 1 {
		t.Errorf("member has %d devices after retries, want 1", len(d))
	}
}

func TestShareCountsTheDataSharedToday(t *testing.T) {
	f := verified(t)
	f.share.SetSharing(t.Context(), true)
	a := f.agents.running(t)
	a.set(func(s *agent.Stats) { s.BytesIn, s.BytesOut = 600_000, 400_000 })
	if got := f.share.Status().BytesToday; got != 1_000_000 {
		t.Fatalf("shared today = %d, want 1000000", got)
	}

	// A new agent counts from zero again; today's count carries on.
	f.share.SetSharing(t.Context(), false)
	f.share.SetSharing(t.Context(), true)
	b := f.agents.running(t)
	b.set(func(s *agent.Stats) { s.BytesIn = 250_000 })
	if got := f.share.Status().BytesToday; got != 1_250_000 {
		t.Fatalf("shared today after turning Share off and on = %d, want 1250000", got)
	}

	// Past midnight, local time, a new day starts.
	f.clock.Set(time.Date(2026, 11, 14, 0, 0, 5, 0, time.Local))
	b.set(func(s *agent.Stats) { s.BytesIn = 300_000 })
	if got := f.share.Status().BytesToday; got != 50_000 {
		t.Errorf("shared on the next day = %d, want 50000", got)
	}
}

func TestTodaysCountSurvivesARestart(t *testing.T) {
	f := verified(t)
	f.share.SetSharing(t.Context(), true)
	a := f.agents.running(t)
	a.set(func(s *agent.Stats) { s.BytesOut = 700_000 })
	if err := f.share.ServiceShutdown(); err != nil { // the app quits
		t.Fatalf("ServiceShutdown: %v", err)
	}

	f.share = f.open(t)
	f.share.SetSharing(t.Context(), true)

	if got := f.share.Status().BytesToday; got != 700_000 {
		t.Errorf("shared today after a restart = %d, want 700000", got)
	}
}

func TestSigningOutTurnsShareOff(t *testing.T) {
	f := verified(t)
	auth := newAuth(t, f.api, f.dir)
	auth.BeforeSignOut(f.share.SignOut)
	f.share.SetSharing(t.Context(), true)
	a := f.agents.running(t)
	a.set(func(s *agent.Stats) { s.BytesIn = 1_000 })

	if err := auth.SignOut(t.Context()); err != nil {
		t.Fatalf("SignOut: %v", err)
	}

	select {
	case <-a.stopped:
	default:
		t.Error("the agent still runs after sign-out")
	}
	if lines := f.api.Lines(t, email); lines[0].SharingEnabled {
		t.Error("the API still has sharing on")
	}
	if got := f.share.Status(); got.State != services.ShareOff || got.BytesToday != 0 {
		t.Errorf("status = %+v, want off with nothing shared today", got)
	}
}

func TestShareComesBackOnAfterARestart(t *testing.T) {
	f := verified(t)
	f.share.SetSharing(t.Context(), true)
	f.agents.nth(t, 1)
	if err := f.share.ServiceShutdown(); err != nil { // the app quits
		t.Fatalf("ServiceShutdown: %v", err)
	}

	f.share = f.open(t)
	if err := f.share.ServiceStartup(t.Context(), application.ServiceOptions{}); err != nil {
		t.Fatalf("ServiceStartup: %v", err)
	}

	if got := f.share.Status(); !got.Sharing {
		t.Errorf("status after start-up = %+v, want Share on", got)
	}
	f.agents.nth(t, 2)
}

func TestShareStaysOffAfterARestart(t *testing.T) {
	f := verified(t)
	f.share.SetSharing(t.Context(), true)
	f.agents.nth(t, 1)
	f.share.SetSharing(t.Context(), false)

	f.share = f.open(t)
	if err := f.share.ServiceStartup(t.Context(), application.ServiceOptions{}); err != nil {
		t.Fatalf("ServiceStartup: %v", err)
	}

	// ServiceStartup decides at once, so Share is off and stays off.
	if got := f.share.Status(); got.State != services.ShareOff || got.Sharing {
		t.Errorf("status = %+v, want off", got)
	}
	if err := f.share.ServiceShutdown(); err != nil {
		t.Fatalf("ServiceShutdown: %v", err)
	}
	if n := f.agents.count(); n != 1 {
		t.Errorf("%d agents ran, want only the first", n)
	}
}

func TestStartUpDoesNotOverrideTheMembersChoice(t *testing.T) {
	f := verified(t)
	f.share.SetSharing(t.Context(), true)
	f.agents.nth(t, 1)
	if err := f.share.ServiceShutdown(); err != nil { // quits with Share on
		t.Fatalf("ServiceShutdown: %v", err)
	}

	f.share = f.open(t)
	// The member turns Share off from the tray before start-up resumes it.
	f.share.SetSharing(t.Context(), false)
	if err := f.share.ServiceStartup(t.Context(), application.ServiceOptions{}); err != nil {
		t.Fatalf("ServiceStartup: %v", err)
	}

	if got := f.share.Status(); got.Sharing {
		t.Errorf("status after start-up = %+v, want Share off as the member chose", got)
	}
}

func TestATurnOffTheAPIMissedIsSentAtTheNextStart(t *testing.T) {
	f := verified(t)
	f.share.SetSharing(t.Context(), true)
	f.agents.nth(t, 1)
	f.api.SetOffline(true)
	f.share.SetSharing(t.Context(), false)
	f.api.SetOffline(false)
	if lines := f.api.Lines(t, email); !lines[0].SharingEnabled {
		t.Fatal("the turn-off reached the API although it was offline")
	}

	f.share = f.open(t)
	if err := f.share.ServiceStartup(t.Context(), application.ServiceOptions{}); err != nil {
		t.Fatalf("ServiceStartup: %v", err)
	}

	eventually(t, "the API has sharing off", func() bool { return !f.api.Lines(t, email)[0].SharingEnabled })
}

func TestShareRegistersAgainAfterTheDeviceWasRemoved(t *testing.T) {
	f := verified(t)
	f.share.SetSharing(t.Context(), true)
	f.agents.nth(t, 1)
	f.share.SetSharing(t.Context(), false)
	f.api.RemoveDevice(f.api.Devices(t, email)[0].ID)

	got := f.share.SetSharing(t.Context(), true)

	if got.State != services.ShareConnecting {
		t.Errorf("status = %+v, want connecting", got)
	}
	f.agents.nth(t, 2)
	if d := f.api.Devices(t, email); len(d) != 1 {
		t.Errorf("member has %d devices, want 1", len(d))
	}
	if l := f.api.Lines(t, email); len(l) != 1 || !l[0].SharingEnabled {
		t.Errorf("lines = %+v, want a new line, sharing", l)
	}
}

func TestShareTellsListenersWhatChanged(t *testing.T) {
	f := verified(t)
	var mu sync.Mutex
	var seen []services.ShareState
	f.share.OnStatus(func(st services.ShareStatus) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, st.State)
	})
	states := func() []services.ShareState {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
	if err := f.share.ServiceStartup(t.Context(), application.ServiceOptions{}); err != nil {
		t.Fatalf("ServiceStartup: %v", err)
	}

	f.share.SetSharing(t.Context(), true)
	f.agents.nth(t, 1).set(func(s *agent.Stats) { s.State = agent.StateConnected })
	eventually(t, "listeners hear online", func() bool {
		got := states()
		return len(got) > 0 && got[len(got)-1] == services.ShareOnline
	})
	f.share.SetSharing(t.Context(), false)

	want := []services.ShareState{services.ShareConnecting, services.ShareOnline, services.ShareOff}
	if got := states(); !slices.Equal(got, want) {
		t.Errorf("listeners heard %v, want %v", got, want)
	}
}

func TestShareStopsWhenTheAPIRefusesTheLine(t *testing.T) {
	f := verified(t)
	// The identity check was withdrawn between /v1/me and the line.
	f.api.FailNext("POST /v1/lines", http.StatusForbidden, "identity_required")

	got := f.share.SetSharing(t.Context(), true)

	if got.State != services.ShareError || got.Error != services.ErrIdentityRequired || got.Sharing {
		t.Errorf("status = %+v, want error %q with the switch off", got, services.ErrIdentityRequired)
	}
}

func TestShareNeedsASignedInMember(t *testing.T) {
	f := verified(t)
	if err := newAuth(t, f.api, f.dir).SignOut(t.Context()); err != nil {
		t.Fatalf("SignOut: %v", err)
	}

	got := f.share.SetSharing(t.Context(), true)

	if got.State != services.ShareError || got.Error != services.ErrSignedOut {
		t.Errorf("status = %+v, want error %q", got, services.ErrSignedOut)
	}
}

// Turning Share on before signing in fails with signed_out. That error is
// about the session that was missing, so signing in clears it; the window
// would otherwise read it as the new session being gone, and go back to
// sign-in, find the member signed in, and read it again, over and over.
func TestSigningInClearsTheSignedOutError(t *testing.T) {
	f := verified(t)
	auth := newAuth(t, f.api, f.dir)
	if err := auth.SignOut(t.Context()); err != nil {
		t.Fatalf("SignOut: %v", err)
	}
	if got := f.share.SetSharing(t.Context(), true); got.Error != services.ErrSignedOut {
		t.Fatalf("status before sign-in = %+v, want error %q", got, services.ErrSignedOut)
	}

	auth.AfterSignIn(f.share.SignIn)
	signIn(t, f.api, auth, email)

	if got := f.share.Status(); got.State != services.ShareOff || got.Error != "" || got.Sharing {
		t.Errorf("status after sign-in = %+v, want off with no error", got)
	}
}

// A listener may wait for the main thread, as the tray's menu updates do,
// while the main thread runs ServiceShutdown. Share must not wait for it.
func TestAListenerThatBlocksHoldsNothingUp(t *testing.T) {
	f := verified(t)
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	f.share.OnStatus(func(services.ShareStatus) { <-unblock })
	if err := f.share.ServiceStartup(t.Context(), application.ServiceOptions{}); err != nil {
		t.Fatalf("ServiceStartup: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.share.SetSharing(t.Context(), true)
		f.agents.nth(t, 1).set(func(s *agent.Stats) { s.State = agent.StateConnected })
		time.Sleep(20 * time.Millisecond) // a few status ticks
		f.share.SetSharing(t.Context(), false)
		_ = f.share.ServiceShutdown()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Share waited for a blocked listener")
	}
}

func TestASessionEndedElsewhereStopsShare(t *testing.T) {
	f := verified(t)
	auth := newAuth(t, f.api, f.dir)
	auth.BeforeSignOut(f.share.SignOut)
	f.share.SetSharing(t.Context(), true)
	a := f.agents.nth(t, 1)

	f.api.RevokeAll() // signed out everywhere from another device
	if me, err := auth.Current(t.Context()); err != nil || me != nil {
		t.Fatalf("Current = %+v, %v; want nobody", me, err)
	}

	select {
	case <-a.stopped:
	default:
		t.Error("the agent still runs although nobody is signed in")
	}
	if got := f.share.Status(); got.Sharing {
		t.Errorf("status = %+v, want Share off", got)
	}
}

func TestTurningOffAgainSendsATurnOffTheAPIMissed(t *testing.T) {
	f := verified(t)
	f.share.SetSharing(t.Context(), true)
	f.agents.nth(t, 1)
	f.api.SetOffline(true)
	f.share.SetSharing(t.Context(), false)
	f.api.SetOffline(false)

	// Share is off already; the member signs out, which turns it off again.
	f.share.SignOut(t.Context())

	if lines := f.api.Lines(t, email); lines[0].SharingEnabled {
		t.Error("the API still has sharing on")
	}
}

func TestTodaysCountComesBackAfterSigningInAgain(t *testing.T) {
	f := verified(t)
	auth := newAuth(t, f.api, f.dir)
	auth.BeforeSignOut(f.share.SignOut)
	auth.AfterSignIn(f.share.SignIn)
	f.share.SetSharing(t.Context(), true)
	f.agents.nth(t, 1).set(func(s *agent.Stats) { s.BytesIn = 2_000_000 })
	f.share.Status()
	if err := auth.SignOut(t.Context()); err != nil {
		t.Fatalf("SignOut: %v", err)
	}

	signIn(t, f.api, auth, email)

	if got := f.share.Status(); got.BytesToday != 2_000_000 || got.Sharing {
		t.Errorf("status = %+v, want 2000000 bytes shared today and Share off", got)
	}
}

func TestShareWaitsAsLongAsTheAPIAsks(t *testing.T) {
	f := verified(t)
	// Ten new devices today already: the API asks to wait an hour.
	f.api.FailNextRetryAfter("POST /v1/devices", http.StatusTooManyRequests, "rate_limited", time.Hour)

	got := f.share.SetSharing(t.Context(), true)

	if got.State != services.ShareOffline || got.Error != services.ErrRateLimited {
		t.Errorf("status = %+v, want offline with error %q", got, services.ErrRateLimited)
	}
	time.Sleep(100 * time.Millisecond) // ten times RetryMax
	n := 0
	for _, r := range f.api.Requests() {
		if r == "POST /v1/devices" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("POST /v1/devices sent %d times, want once while the API asks to wait", n)
	}
}
