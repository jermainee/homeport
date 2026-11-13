// Package services holds the Wails services the frontend calls. The webview
// never talks to the API itself: every call goes through a service here, so
// the session token never reaches JavaScript.
package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/jermainee/homeport/desktop-app/internal/apiclient"
	"github.com/jermainee/homeport/desktop-app/internal/profile"
)

// AuthService signs the member in with an email link and keeps the session
// in the profile.
type AuthService struct {
	api     *apiclient.Client
	profile *profile.Profile

	mu      sync.Mutex
	pending *pendingLogin
}

// pendingLogin is a sign-in the member has started but not yet approved.
type pendingLogin struct {
	email     string
	requestID string
	token     string
	id        string // login_request_id, empty until the API accepted it
}

// NewAuthService returns an AuthService that calls api and keeps the
// session in p.
func NewAuthService(api *apiclient.Client, p *profile.Profile) *AuthService {
	return &AuthService{api: api, profile: p}
}

// LoginStart is a started sign-in, as the sign-in screen shows it.
type LoginStart struct {
	Email     string    `json:"email"`
	MatchCode string    `json:"matchCode"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Start emails a sign-in link to email and returns the match code the
// member compares on the page behind the link.
func (s *AuthService) Start(ctx context.Context, email string) (LoginStart, error) {
	email = strings.TrimSpace(email)
	s.mu.Lock()
	defer s.mu.Unlock()

	// A retry after an unknown outcome reuses the attempt, so the API
	// replays its first answer instead of emailing a second link.
	p := s.pending
	if p == nil || p.id != "" || p.email != email {
		p = &pendingLogin{email: email, requestID: uuid.NewV7().String(), token: newSessionToken()}
	}
	s.pending = p
	lr, err := s.api.CreateLoginRequest(ctx, p.requestID, email, sha256Hex(p.token))
	if err != nil {
		if !retryable(err) {
			s.pending = nil
		}
		return LoginStart{}, serviceError(err, ErrInvalidEmail)
	}
	p.id = lr.ID
	return LoginStart{Email: email, MatchCode: lr.MatchCode, ExpiresAt: lr.ExpiresAt}, nil
}

// newSessionToken creates a session token: "hpm_" and 32 random bytes in
// base64url, 47 characters in all.
func newSessionToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails.
	return "hpm_" + base64.RawURLEncoding.EncodeToString(b)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Cancel forgets the sign-in in progress, so the member can start over.
func (s *AuthService) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = nil
}

// LoginStatus is where a started sign-in stands.
type LoginStatus string

const (
	// LoginPending: the member hasn't approved the sign-in yet. Poll again.
	LoginPending LoginStatus = "pending"
	// LoginApproved: the member approved; the session is saved.
	LoginApproved LoginStatus = "approved"
	// LoginExpired: the link ran out before the member approved it.
	LoginExpired LoginStatus = "expired"
)

// PollResult is the answer to one poll of a started sign-in.
type PollResult struct {
	Status LoginStatus `json:"status"`
}

// Poll asks the API once whether the member has approved the sign-in. Once
// they have, it saves the session in the profile.
func (s *AuthService) Poll(ctx context.Context) (PollResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := s.pending
	if p == nil || p.id == "" {
		return PollResult{}, &Error{Code: ErrNoSignIn}
	}
	st, err := s.api.GetLoginRequest(ctx, p.id, p.token)
	if apiclient.IsStatus(err, http.StatusGone) {
		s.pending = nil
		return PollResult{Status: LoginExpired}, nil
	}
	if err != nil {
		if !retryable(err) {
			s.pending = nil
		}
		return PollResult{}, serviceError(err, "")
	}
	if !st.Approved {
		return PollResult{Status: LoginPending}, nil
	}
	if err := s.profile.SaveSession(profile.Session{Token: p.token, MemberID: st.MemberID, ExpiresAt: st.ExpiresAt}); err != nil {
		return PollResult{}, serviceError(err, "")
	}
	s.pending = nil
	return PollResult{Status: LoginApproved}, nil
}

// Member is the signed-in member, as the home screen shows them.
type Member struct {
	MemberID       string  `json:"memberId"`
	Email          string  `json:"email"`
	IdentityStatus string  `json:"identityStatus"`
	Credits        Credits `json:"credits"`
}

// Credits is the member's balance by kind.
type Credits struct {
	Purchased      int64 `json:"purchased"`
	EarnedPending  int64 `json:"earnedPending"`
	EarnedCashable int64 `json:"earnedCashable"`
}

// Current returns the signed-in member, or nil when nobody is signed in.
func (s *AuthService) Current(ctx context.Context) (*Member, error) {
	sess, ok, err := s.profile.Session()
	if err != nil || !ok {
		return nil, serviceError(err, "")
	}
	m, err := s.api.Me(ctx, sess.Token)
	if apiclient.IsStatus(err, http.StatusUnauthorized) {
		// Signed out elsewhere, or the session ran out.
		return nil, serviceError(s.profile.ClearSession(), "")
	}
	if err != nil {
		return nil, serviceError(err, "")
	}
	return &Member{
		MemberID:       m.MemberID,
		Email:          m.Email,
		IdentityStatus: m.IdentityStatus,
		Credits: Credits{
			Purchased:      m.Credits.Purchased,
			EarnedPending:  m.Credits.EarnedPending,
			EarnedCashable: m.Credits.EarnedCashable,
		},
	}, nil
}

// retryable reports whether a request may succeed if sent again unchanged:
// it never reached the API, the API failed, or it asked to slow down.
func retryable(err error) bool {
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.Status >= 500 || apiErr.Status == http.StatusTooManyRequests
}

// SignOut ends the session on the API and forgets it here. The local
// session is cleared even when the API can't be reached: the token exists
// only in this profile, so nobody can use it afterwards.
func (s *AuthService) SignOut(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = nil

	sess, ok, _ := s.profile.Session() // an unreadable file is cleared below
	if ok {
		ctx, cancel := context.WithTimeout(ctx, signOutTimeout)
		_ = s.api.DeleteSession(ctx, sess.Token) // best effort, see above
		cancel()
	}
	return serviceError(s.profile.ClearSession(), "")
}

const signOutTimeout = 5 * time.Second
