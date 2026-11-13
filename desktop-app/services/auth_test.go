package services_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jermainee/homeport/desktop-app/internal/apiclient"
	"github.com/jermainee/homeport/desktop-app/internal/fakeapi"
	"github.com/jermainee/homeport/desktop-app/internal/profile"
	"github.com/jermainee/homeport/desktop-app/services"
)

const email = "member@example.com"

// newAuth returns an AuthService that talks to api and keeps its state in
// the profile directory dir, as the app does for HOMEPORT_PROFILE=dir.
func newAuth(t *testing.T, api *fakeapi.Server, dir string) *services.AuthService {
	t.Helper()
	p, err := profile.Open(dir)
	if err != nil {
		t.Fatalf("open profile: %v", err)
	}
	return services.NewAuthService(apiclient.New(api.URL, api.Client()), p)
}

func TestStartShowsTheMatchCodeFromTheAPI(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())

	got, err := auth.Start(t.Context(), email)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if want := api.MatchCode(t, email); got.MatchCode != want {
		t.Errorf("match code = %q, want %q", got.MatchCode, want)
	}
	if got.Email != email {
		t.Errorf("email = %q, want %q", got.Email, email)
	}
}

func TestPollIsPendingUntilTheMemberApproves(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())
	if _, err := auth.Start(t.Context(), email); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got, err := auth.Poll(t.Context())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got.Status != services.LoginPending {
		t.Fatalf("status before approval = %q, want %q", got.Status, services.LoginPending)
	}

	api.Approve(t, email)
	got, err = auth.Poll(t.Context())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got.Status != services.LoginApproved {
		t.Fatalf("status after approval = %q, want %q", got.Status, services.LoginApproved)
	}

	me, err := auth.Current(t.Context())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if me == nil || me.Email != email {
		t.Fatalf("Current = %+v, want member %s", me, email)
	}
}

func TestCurrentIsNilBeforeSignIn(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())

	me, err := auth.Current(t.Context())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if me != nil {
		t.Fatalf("Current = %+v, want nil", me)
	}
	if reqs := api.Requests(); len(reqs) != 0 {
		t.Errorf("signed-out Current called the API: %v", reqs)
	}
}

// signIn runs the whole email sign-in for email on auth.
func signIn(t *testing.T, api *fakeapi.Server, auth *services.AuthService, email string) {
	t.Helper()
	if _, err := auth.Start(t.Context(), email); err != nil {
		t.Fatalf("Start: %v", err)
	}
	api.Approve(t, email)
	got, err := auth.Poll(t.Context())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got.Status != services.LoginApproved {
		t.Fatalf("status = %q, want %q", got.Status, services.LoginApproved)
	}
}

func TestSessionIsKeptPerProfile(t *testing.T) {
	api := fakeapi.New(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	signIn(t, api, newAuth(t, api, dirA), "a@example.com")
	signIn(t, api, newAuth(t, api, dirB), "b@example.com")

	// The app restarts with each profile.
	for dir, want := range map[string]string{dirA: "a@example.com", dirB: "b@example.com"} {
		me, err := newAuth(t, api, dir).Current(t.Context())
		if err != nil {
			t.Fatalf("Current: %v", err)
		}
		if me == nil || me.Email != want {
			t.Errorf("after restart, Current = %+v, want %s", me, want)
		}
	}

	// A third, fresh profile is signed out.
	me, err := newAuth(t, api, t.TempDir()).Current(t.Context())
	if err != nil || me != nil {
		t.Errorf("fresh profile: Current = %+v, %v; want nil, nil", me, err)
	}
}

func TestSignOutEndsTheSessionEverywhere(t *testing.T) {
	api := fakeapi.New(t)
	dir := t.TempDir()
	auth := newAuth(t, api, dir)
	signIn(t, api, auth, email)

	if err := auth.SignOut(t.Context()); err != nil {
		t.Fatalf("SignOut: %v", err)
	}

	if n := api.ActiveSessions(); n != 0 {
		t.Errorf("API still has %d active sessions", n)
	}
	me, err := newAuth(t, api, dir).Current(t.Context())
	if err != nil || me != nil {
		t.Errorf("after restart, Current = %+v, %v; want nil, nil", me, err)
	}
}

func TestSignOutWorksOffline(t *testing.T) {
	api := fakeapi.New(t)
	dir := t.TempDir()
	auth := newAuth(t, api, dir)
	signIn(t, api, auth, email)
	api.FailNext("DELETE /v1/auth/session", 0, "")

	if err := auth.SignOut(t.Context()); err != nil {
		t.Fatalf("SignOut: %v", err)
	}

	me, err := newAuth(t, api, dir).Current(t.Context())
	if err != nil || me != nil {
		t.Errorf("after restart, Current = %+v, %v; want nil, nil", me, err)
	}
}

func TestCurrentForgetsASessionTheAPIRevoked(t *testing.T) {
	api := fakeapi.New(t)
	dir := t.TempDir()
	signIn(t, api, newAuth(t, api, dir), email)
	api.RevokeAll() // signed out on another device, or the session ran out

	me, err := newAuth(t, api, dir).Current(t.Context())
	if err != nil || me != nil {
		t.Fatalf("Current = %+v, %v; want nil, nil", me, err)
	}
	n := len(api.Requests())
	if me, _ := newAuth(t, api, dir).Current(t.Context()); me != nil || len(api.Requests()) != n {
		t.Errorf("the revoked session was not forgotten: Current = %+v", me)
	}
}

func TestPollReportsAnExpiredLink(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())
	if _, err := auth.Start(t.Context(), email); err != nil {
		t.Fatalf("Start: %v", err)
	}
	api.Expire(t, email)

	got, err := auth.Poll(t.Context())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got.Status != services.LoginExpired {
		t.Errorf("status = %q, want %q", got.Status, services.LoginExpired)
	}
}

func TestRetriedStartReusesTheLoginRequest(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())
	api.LoseNextResponse("POST /v1/auth/login-requests")

	if _, err := auth.Start(t.Context(), email); err == nil {
		t.Fatal("Start succeeded although the response was lost")
	}
	got, err := auth.Start(t.Context(), email)
	if err != nil {
		t.Fatalf("retried Start: %v", err)
	}

	if n := api.LoginRequests(); n != 1 {
		t.Errorf("API has %d login requests, want 1 (one email, not two)", n)
	}
	if want := api.MatchCode(t, email); got.MatchCode != want {
		t.Errorf("match code = %q, want %q", got.MatchCode, want)
	}
	api.Approve(t, email)
	if res, err := auth.Poll(t.Context()); err != nil || res.Status != services.LoginApproved {
		t.Errorf("Poll = %+v, %v; want approved", res, err)
	}
}

func TestStartWithAnotherEmailStartsOver(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())
	api.LoseNextResponse("POST /v1/auth/login-requests")
	_, _ = auth.Start(t.Context(), "typo@example.con")

	if _, err := auth.Start(t.Context(), email); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if n := api.LoginRequests(); n != 2 {
		t.Errorf("API has %d login requests, want 2", n)
	}
}

func TestStartErrorsCarryACodeTheUICanExplain(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string // API error code; empty drops the connection
		want   services.ErrorCode
	}{
		{"unreachable API", 0, "", services.ErrOffline},
		{"bad email", http.StatusBadRequest, "invalid_request", services.ErrInvalidEmail},
		{"too many sign-ins", http.StatusTooManyRequests, "rate_limited", services.ErrRateLimited},
		{"email can't be sent", http.StatusServiceUnavailable, "email_unavailable", services.ErrEmailUnavailable},
		{"API failure", http.StatusInternalServerError, "internal_error", services.ErrUnexpected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := fakeapi.New(t)
			auth := newAuth(t, api, t.TempDir())
			api.FailNext("POST /v1/auth/login-requests", tc.status, tc.code)

			_, err := auth.Start(t.Context(), email)

			var svcErr *services.Error
			if !errors.As(err, &svcErr) {
				t.Fatalf("Start error = %v, want a *services.Error", err)
			}
			if svcErr.Code != tc.want {
				t.Errorf("code = %q, want %q", svcErr.Code, tc.want)
			}
			// Wails hands the frontend the error marshalled as JSON.
			b, _ := json.Marshal(err)
			if want := `{"code":"` + string(tc.want) + `"}`; string(b) != want {
				t.Errorf("error JSON = %s, want %s", b, want)
			}
		})
	}
}

func TestPollWithoutStartIsAnError(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())

	_, err := auth.Poll(t.Context())

	var svcErr *services.Error
	if !errors.As(err, &svcErr) || svcErr.Code != services.ErrNoSignIn {
		t.Errorf("Poll error = %v, want code %q", err, services.ErrNoSignIn)
	}
}

func TestCancelForgetsTheSignIn(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())
	if _, err := auth.Start(t.Context(), email); err != nil {
		t.Fatalf("Start: %v", err)
	}

	auth.Cancel()
	api.Approve(t, email)

	_, err := auth.Poll(t.Context())
	var svcErr *services.Error
	if !errors.As(err, &svcErr) || svcErr.Code != services.ErrNoSignIn {
		t.Errorf("Poll after Cancel = %v, want code %q", err, services.ErrNoSignIn)
	}
}

func TestCancelDoesNotWaitForAPollAndDiscardsItsAnswer(t *testing.T) {
	api := fakeapi.New(t)
	dir := t.TempDir()
	auth := newAuth(t, api, dir)
	if _, err := auth.Start(t.Context(), email); err != nil {
		t.Fatalf("Start: %v", err)
	}
	release := api.HoldNext("GET /v1/auth/login-requests/")
	defer release()
	polled := make(chan error, 1)
	go func() {
		_, err := auth.Poll(t.Context())
		polled <- err
	}()
	waitFor(t, func() bool { return len(api.Requests()) == 2 }) // the poll reached the API

	// The member confirms in the browser just as they go back in the app.
	api.Approve(t, email)
	cancelled := make(chan struct{})
	go func() { auth.Cancel(); close(cancelled) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Cancel waited for the poll in flight")
	}
	release()

	err := <-polled
	var svcErr *services.Error
	if !errors.As(err, &svcErr) || svcErr.Code != services.ErrNoSignIn {
		t.Errorf("Poll = %v, want code %q", err, services.ErrNoSignIn)
	}
	if me, err := newAuth(t, api, dir).Current(t.Context()); err != nil || me != nil {
		t.Errorf("the abandoned sign-in was saved: Current = %+v, %v", me, err)
	}
}

func TestFailedResendKeepsTheFirstSignIn(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())
	first, err := auth.Start(t.Context(), email)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	api.FailNext("POST /v1/auth/login-requests", http.StatusTooManyRequests, "rate_limited")
	if _, err := auth.Start(t.Context(), email); err == nil {
		t.Fatal("resend succeeded, want rate_limited")
	}

	// The member opens the first email after all.
	if got := api.MatchCode(t, email); got != first.MatchCode {
		t.Fatalf("latest login has code %s, want the first one %s", got, first.MatchCode)
	}
	api.Approve(t, email)
	if res, err := auth.Poll(t.Context()); err != nil || res.Status != services.LoginApproved {
		t.Errorf("Poll = %+v, %v; want approved", res, err)
	}
}

func TestPollGivesUpWhenTheAPIRefusesTheSignIn(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())
	if _, err := auth.Start(t.Context(), email); err != nil {
		t.Fatalf("Start: %v", err)
	}
	api.FailNext("GET /v1/auth/login-requests/", http.StatusNotFound, "not_found")

	_, err := auth.Poll(t.Context())

	var svcErr *services.Error
	if !errors.As(err, &svcErr) || svcErr.Code != services.ErrNoSignIn {
		t.Errorf("Poll = %v, want code %q", err, services.ErrNoSignIn)
	}
}

func TestStartExplainsAClockThatIsOff(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())
	// The API rejects request_ids from more than an hour in its future.
	api.FailNextAt("POST /v1/auth/login-requests", http.StatusBadRequest, "invalid_request", time.Now().Add(-2*time.Hour))

	_, err := auth.Start(t.Context(), email)

	var svcErr *services.Error
	if !errors.As(err, &svcErr) || svcErr.Code != services.ErrClockSkew {
		t.Errorf("Start = %v, want code %q", err, services.ErrClockSkew)
	}
}

func TestStartRetriesAnExpiredRequestWithAFreshOne(t *testing.T) {
	api := fakeapi.New(t)
	auth := newAuth(t, api, t.TempDir())
	api.FailNext("POST /v1/auth/login-requests", http.StatusBadRequest, "request_id_expired")

	_, err := auth.Start(t.Context(), email)
	var svcErr *services.Error
	if !errors.As(err, &svcErr) || svcErr.Code != services.ErrUnexpected {
		t.Fatalf("Start = %v, want code %q", err, services.ErrUnexpected)
	}
	if _, err := auth.Start(t.Context(), email); err != nil {
		t.Errorf("second Start: %v", err)
	}
}

func TestCurrentTreatsAnUnreadableSessionAsSignedOut(t *testing.T) {
	api := fakeapi.New(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte("{trunc"), 0o600); err != nil {
		t.Fatal(err)
	}

	me, err := newAuth(t, api, dir).Current(t.Context())

	if err != nil || me != nil {
		t.Errorf("Current = %+v, %v; want nil, nil", me, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "session.json")); !os.IsNotExist(err) {
		t.Errorf("the unreadable session file is still there: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
