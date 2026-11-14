package services

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/jermainee/homeport/desktop-app/internal/apiclient"
)

// ErrorCode tells the frontend what went wrong, so it can say so in words
// the member understands.
type ErrorCode string

const (
	// ErrOffline: the API could not be reached.
	ErrOffline ErrorCode = "offline"
	// ErrInvalidEmail: the API rejected the email address.
	ErrInvalidEmail ErrorCode = "invalid_email"
	// ErrClockSkew: this computer's clock is more than an hour off, so the
	// API rejects the request's timestamp.
	ErrClockSkew ErrorCode = "clock_skew"
	// ErrRateLimited: too many attempts; wait a moment.
	ErrRateLimited ErrorCode = "rate_limited"
	// ErrEmailUnavailable: the API can't send sign-in links right now.
	ErrEmailUnavailable ErrorCode = "email_unavailable"
	// ErrNoSignIn: there is no sign-in in progress to poll.
	ErrNoSignIn ErrorCode = "no_sign_in"
	// ErrIdentityRequired: the member hasn't passed the identity check, so
	// Share and Use stay off.
	ErrIdentityRequired ErrorCode = "identity_required"
	// ErrGatewayUnavailable: no gateway can take the line yet.
	ErrGatewayUnavailable ErrorCode = "gateway_unavailable"
	// ErrSignedOut: the session is gone, so the member has to sign in again.
	ErrSignedOut ErrorCode = "signed_out"
	// ErrLineUnavailable: the line stopped sharing, or its owner left.
	ErrLineUnavailable ErrorCode = "line_unavailable"
	// ErrLineFull: the line serves as many members as it can.
	ErrLineFull ErrorCode = "line_full"
	// ErrLineNotAllowed: the line is the member's own, or linked to them.
	ErrLineNotAllowed ErrorCode = "line_not_allowed"
	// ErrConfigGone: the device's config was shown once and is gone; the
	// private key in it was never stored.
	ErrConfigGone ErrorCode = "config_gone"
	// ErrInvalidName: a device needs a name of 1 to 64 characters.
	ErrInvalidName ErrorCode = "invalid_name"
	// ErrUnexpected: anything else. The message has the details for logs.
	ErrUnexpected ErrorCode = "unexpected"
)

// Error is how a service call fails. Wails marshals it to JSON, and the
// frontend reads the code from the call error's cause; the wrapped error
// stays in Go and only shapes the log message.
type Error struct {
	Code ErrorCode `json:"code"`
	err  error
}

func (e *Error) Error() string {
	if e.err == nil {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.err.Error()
}

func (e *Error) Unwrap() error { return e.err }

// serviceError turns an error from the API client or the profile into an
// *Error the frontend can explain.
func serviceError(err error) error {
	if err == nil {
		return nil
	}
	var svcErr *Error
	if errors.As(err, &svcErr) {
		return err
	}
	var apiErr *apiclient.Error
	var urlErr *url.Error
	code := ErrUnexpected
	switch {
	case errors.As(err, &apiErr):
		switch {
		case apiErr.Status == http.StatusUnauthorized:
			code = ErrSignedOut
		case apiErr.Status == http.StatusTooManyRequests:
			code = ErrRateLimited
		case apiErr.Code == "email_unavailable":
			code = ErrEmailUnavailable
		case apiErr.Code == "identity_required":
			code = ErrIdentityRequired
		}
	case errors.As(err, &urlErr):
		code = ErrOffline
	}
	return &Error{Code: code, err: err}
}

// maxClockSkew is how far the API lets a request_id's UUIDv7 timestamp run
// ahead of its clock.
const maxClockSkew = time.Hour

// startError explains why starting a sign-in failed. The app makes every
// field but the email, so `invalid_request` is about the email, unless the
// API's clock shows that ours is too far off for the request_id.
func startError(err error) error {
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest && apiErr.Code == "invalid_request" {
		if !apiErr.ServerTime.IsZero() && time.Since(apiErr.ServerTime).Abs() > maxClockSkew {
			return &Error{Code: ErrClockSkew, err: err}
		}
		return &Error{Code: ErrInvalidEmail, err: err}
	}
	return serviceError(err)
}
