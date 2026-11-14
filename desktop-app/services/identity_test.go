package services_test

import (
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/jermainee/homeport/desktop-app/internal/apiclient"
	"github.com/jermainee/homeport/desktop-app/internal/fakeapi"
	"github.com/jermainee/homeport/desktop-app/internal/profile"
	"github.com/jermainee/homeport/desktop-app/services"
)

// browser stands in for the system browser: it records every URL the app
// opens.
type browser struct {
	mu     sync.Mutex
	opened []string
}

func (b *browser) Open(url string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opened = append(b.opened, url)
	return nil
}

func (b *browser) URLs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.opened)
}

// identityFixture is a signed-in member with an IdentityService on their
// profile.
type identityFixture struct {
	api      *fakeapi.Server
	browser  *browser
	identity *services.IdentityService
}

func newIdentity(t *testing.T) *identityFixture {
	t.Helper()
	api := fakeapi.New(t)
	dir := t.TempDir()
	signIn(t, api, newAuth(t, api, dir), email)
	p, err := profile.Open(dir)
	if err != nil {
		t.Fatalf("open profile: %v", err)
	}
	b := &browser{}
	return &identityFixture{
		api:      api,
		browser:  b,
		identity: services.NewIdentityService(apiclient.New(api.URL, api.Client()), p, b.Open),
	}
}

func TestVerifyIdentityOpensStripeInTheBrowser(t *testing.T) {
	f := newIdentity(t)

	got, err := f.identity.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	sessions := f.api.IdentitySessions(t, email)
	if len(sessions) != 1 {
		t.Fatalf("identity checks started = %d, want 1", len(sessions))
	}
	if urls := f.browser.URLs(); !slices.Equal(urls, sessions) {
		t.Errorf("browser opened %q, want the check's page %q", urls, sessions)
	}
	if got.State != services.IdentityWaiting {
		t.Errorf("state = %q, want %q while the member is on Stripe's page", got.State, services.IdentityWaiting)
	}
}

func TestVerifyIdentityRetriesWithTheSameRequest(t *testing.T) {
	f := newIdentity(t)
	// The API starts the check, but the answer is lost on the way back.
	f.api.LoseNextResponse("POST /v1/identity/sessions")

	if _, err := f.identity.Start(t.Context()); err == nil {
		t.Fatal("Start succeeded although the answer was lost")
	}
	if _, err := f.identity.Start(t.Context()); err != nil {
		t.Fatalf("Start again: %v", err)
	}

	// The retry got the first check's page, so it doesn't use up another of
	// the member's daily checks.
	if sessions := f.api.IdentitySessions(t, email); len(sessions) != 1 {
		t.Errorf("identity checks started = %d, want 1", len(sessions))
	}
	if urls := f.browser.URLs(); len(urls) != 1 {
		t.Errorf("browser opened %q, want one page", urls)
	}
}

// A retry the next day finds its request_id expired; the app starts the
// check anew instead of failing once.
func TestVerifyIdentityStartsAnewWhenTheRetryIsTooOld(t *testing.T) {
	f := newIdentity(t)
	f.api.LoseNextResponse("POST /v1/identity/sessions")
	if _, err := f.identity.Start(t.Context()); err == nil {
		t.Fatal("Start succeeded although the answer was lost")
	}
	f.api.FailNext("POST /v1/identity/sessions", http.StatusBadRequest, "request_id_expired")

	if _, err := f.identity.Start(t.Context()); err != nil {
		t.Fatalf("Start a day later: %v", err)
	}

	sessions := f.api.IdentitySessions(t, email)
	if urls := f.browser.URLs(); len(sessions) != 2 || !slices.Equal(urls, sessions[1:]) {
		t.Errorf("browser opened %q, want the new check's page of %q", urls, sessions)
	}
}

func TestVerifyIdentityWhenAlreadyVerifiedOpensNothing(t *testing.T) {
	f := newIdentity(t)
	f.api.Verify(t, email)

	got, err := f.identity.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got.State != services.IdentityVerified {
		t.Errorf("state = %q, want %q", got.State, services.IdentityVerified)
	}
	if urls := f.browser.URLs(); len(urls) != 0 {
		t.Errorf("browser opened %q, want nothing", urls)
	}
}

func TestVerifyIdentityExplainsTooManyChecks(t *testing.T) {
	f := newIdentity(t)
	f.api.FailNext("POST /v1/identity/sessions", http.StatusTooManyRequests, "rate_limited")

	_, err := f.identity.Start(t.Context())

	if code := errCode(err); code != services.ErrRateLimited {
		t.Errorf("error = %v, want code %q", err, services.ErrRateLimited)
	}
	if urls := f.browser.URLs(); len(urls) != 0 {
		t.Errorf("browser opened %q, want nothing", urls)
	}
}

// Polling /v1/me after Start shows each step of the check.
func TestIdentityPollFollowsTheCheck(t *testing.T) {
	f := newIdentity(t)
	if _, err := f.identity.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	steps := []struct {
		api  string // what the Stripe webhook set
		want services.IdentityState
	}{
		{"unverified", services.IdentityWaiting},    // still on Stripe's page
		{"processing", services.IdentityProcessing}, // submitted
		{"verified", services.IdentityVerified},     // passed
	}
	for _, step := range steps {
		f.api.SetIdentityStatus(t, email, step.api)
		got, err := f.identity.Poll(t.Context())
		if err != nil {
			t.Fatalf("Poll at %s: %v", step.api, err)
		}
		if got.State != step.want {
			t.Errorf("API at %q: state = %q, want %q", step.api, got.State, step.want)
		}
	}
}

func TestIdentityPollShowsAFailedCheck(t *testing.T) {
	f := newIdentity(t)
	if _, err := f.identity.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.api.SetIdentityStatus(t, email, "processing")
	if _, err := f.identity.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	f.api.SetIdentityStatus(t, email, "requires_input")
	got, err := f.identity.Poll(t.Context())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if got.State != services.IdentityRequiresInput {
		t.Errorf("state = %q, want %q", got.State, services.IdentityRequiresInput)
	}
}

// A member whose last check failed starts a new one. The API still says
// requires_input until Stripe hears from the new check, so that is not the
// new check's outcome.
func TestIdentityPollWaitsForTheNewCheckAfterAFailedOne(t *testing.T) {
	f := newIdentity(t)
	f.api.SetIdentityStatus(t, email, "requires_input")
	if _, err := f.identity.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got, err := f.identity.Poll(t.Context())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got.State != services.IdentityWaiting {
		t.Errorf("state = %q, want %q until the new check reports", got.State, services.IdentityWaiting)
	}

	f.api.SetIdentityStatus(t, email, "processing")
	if _, err := f.identity.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	f.api.SetIdentityStatus(t, email, "requires_input")
	got, err = f.identity.Poll(t.Context())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got.State != services.IdentityRequiresInput {
		t.Errorf("state = %q, want %q once the new check failed", got.State, services.IdentityRequiresInput)
	}
}

// Without a check started in this run, Poll reports what the API says, as
// "Check again" shows it.
func TestIdentityPollWithoutACheckReportsTheAPI(t *testing.T) {
	f := newIdentity(t)

	for _, tc := range []struct {
		api  string
		want services.IdentityState
	}{
		{"unverified", services.IdentityUnverified},
		{"requires_input", services.IdentityRequiresInput},
		{"processing", services.IdentityProcessing},
		{"verified", services.IdentityVerified},
	} {
		f.api.SetIdentityStatus(t, email, tc.api)
		got, err := f.identity.Poll(t.Context())
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if got.State != tc.want {
			t.Errorf("API at %q: state = %q, want %q", tc.api, got.State, tc.want)
		}
	}
}

func TestIdentityPollWhileOfflineSaysSo(t *testing.T) {
	f := newIdentity(t)
	f.api.SetOffline(true)

	_, err := f.identity.Poll(t.Context())

	if code := errCode(err); code != services.ErrOffline {
		t.Errorf("error = %v, want code %q", err, services.ErrOffline)
	}
}

func TestIdentityPollAfterTheSessionEndedSaysSignedOut(t *testing.T) {
	f := newIdentity(t)
	f.api.RevokeAll()

	_, err := f.identity.Poll(t.Context())

	if code := errCode(err); code != services.ErrSignedOut {
		t.Errorf("error = %v, want code %q", err, services.ErrSignedOut)
	}
}

// errCode returns the code of a service error, or "" for nil or another
// error.
func errCode(err error) services.ErrorCode {
	var svcErr *services.Error
	if errors.As(err, &svcErr) {
		return svcErr.Code
	}
	return ""
}
