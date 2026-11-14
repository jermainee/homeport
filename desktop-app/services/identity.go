package services

import (
	"context"
	"errors"
	"sync"

	"github.com/jermainee/homeport/desktop-app/internal/apiclient"
	"github.com/jermainee/homeport/desktop-app/internal/profile"
)

// IdentityState is where the member's identity check stands, as the Share
// and Use screens show it.
type IdentityState string

const (
	// IdentityUnverified: the member hasn't started a check.
	IdentityUnverified IdentityState = "unverified"
	// IdentityWaiting: the check's page is open in the browser, and Stripe
	// hasn't heard back from the member yet.
	IdentityWaiting IdentityState = "waiting"
	// IdentityProcessing: the member sent their ID and selfie; Stripe is
	// checking them.
	IdentityProcessing IdentityState = "processing"
	// IdentityRequiresInput: the last check failed or was abandoned; the
	// member can start another.
	IdentityRequiresInput IdentityState = "requires_input"
	// IdentityVerified: the member passed. Share and Use can turn on.
	IdentityVerified IdentityState = "verified"
)

// IdentityCheck is the answer to Start and Poll.
type IdentityCheck struct {
	State IdentityState `json:"state"`
}

// IdentityService runs the Stripe Identity check that every member passes
// before Share or Use turns on: Start opens the check's page in the system
// browser, and Poll asks the API how it went.
//
// Locking follows AuthService: no API call holds the lock.
type IdentityService struct {
	api     *apiclient.Client
	profile *profile.Profile
	openURL func(string) error

	start retained[struct{}] // a Start whose outcome is unknown, reused on retry

	mu    sync.Mutex
	check *identityCheck // the check started in this run, until its outcome
}

// identityCheck is a check the member started from this app. The API keeps
// the previous check's requires_input until Stripe reports on the new one,
// so a check started from requires_input waits until the status moved.
type identityCheck struct {
	memberID string
	from     string // the API's identity_status when the check started
	moved    bool   // the API reported on this check: processing
}

// NewIdentityService returns an IdentityService that calls api for the
// member signed in to p and opens Stripe's pages with openURL.
func NewIdentityService(api *apiclient.Client, p *profile.Profile, openURL func(string) error) *IdentityService {
	return &IdentityService{api: api, profile: p, openURL: openURL}
}

// Start opens a new identity check in the browser. A member who is
// verified already gets IdentityVerified, and nothing opens.
func (s *IdentityService) Start(ctx context.Context) (IdentityCheck, error) {
	sess, err := session(s.profile)
	if err != nil {
		return IdentityCheck{}, err
	}
	me, err := s.api.Me(ctx, sess.Token)
	if err != nil {
		return IdentityCheck{}, serviceError(err)
	}
	if me.IdentityStatus == string(IdentityVerified) {
		s.forget()
		return IdentityCheck{State: IdentityVerified}, nil
	}

	// A retry after an unknown outcome gets the first check's page again
	// instead of starting a second check.
	var url string
	err = s.start.send(sess.MemberID, nil, nil, func(requestID string, _ struct{}) (err error) {
		url, err = s.api.CreateIdentitySession(ctx, sess.Token, requestID)
		return err
	})
	if apiCode(err) == "identity_already_verified" {
		s.forget()
		return IdentityCheck{State: IdentityVerified}, nil
	}
	if err != nil {
		return IdentityCheck{}, serviceError(err)
	}
	if err := s.openURL(url); err != nil {
		return IdentityCheck{}, &Error{Code: ErrUnexpected, err: err}
	}

	s.mu.Lock()
	s.check = &identityCheck{memberID: sess.MemberID, from: me.IdentityStatus}
	s.mu.Unlock()
	return IdentityCheck{State: IdentityWaiting}, nil
}

// Poll asks the API once how the member's identity check stands.
func (s *IdentityService) Poll(ctx context.Context) (IdentityCheck, error) {
	sess, err := session(s.profile)
	if err != nil {
		return IdentityCheck{}, err
	}
	me, err := s.api.Me(ctx, sess.Token)
	if err != nil {
		return IdentityCheck{}, serviceError(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.check
	if c != nil && c.memberID != sess.MemberID {
		c = nil
	}
	switch me.IdentityStatus {
	case string(IdentityVerified):
		s.check = nil
		return IdentityCheck{State: IdentityVerified}, nil
	case string(IdentityProcessing):
		if c != nil {
			c.moved = true
		}
		return IdentityCheck{State: IdentityProcessing}, nil
	case string(IdentityRequiresInput):
		if c != nil && c.from == string(IdentityRequiresInput) && !c.moved {
			return IdentityCheck{State: IdentityWaiting}, nil
		}
		s.check = nil
		return IdentityCheck{State: IdentityRequiresInput}, nil
	default:
		if c != nil {
			return IdentityCheck{State: IdentityWaiting}, nil
		}
		return IdentityCheck{State: IdentityUnverified}, nil
	}
}

// SignOut forgets the check in progress before the member signs out.
//
//wails:ignore
func (s *IdentityService) SignOut(context.Context) { s.forget() }

func (s *IdentityService) forget() {
	s.start.reset()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.check = nil
}

// session returns the signed-in member's session, or ErrSignedOut.
func session(p *profile.Profile) (profile.Session, error) {
	sess, ok, err := p.Session()
	if err != nil || !ok {
		return profile.Session{}, &Error{Code: ErrSignedOut, err: err}
	}
	return sess, nil
}

// apiCode returns the API's error code, or "" for another error.
func apiCode(err error) string {
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}
