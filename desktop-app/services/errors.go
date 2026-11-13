package services

import (
	"errors"
	"net/http"
	"net/url"

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
	// ErrRateLimited: too many attempts; wait a moment.
	ErrRateLimited ErrorCode = "rate_limited"
	// ErrEmailUnavailable: the API can't send sign-in links right now.
	ErrEmailUnavailable ErrorCode = "email_unavailable"
	// ErrNoSignIn: there is no sign-in in progress to poll.
	ErrNoSignIn ErrorCode = "no_sign_in"
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

// serviceError turns an API client error into an *Error. A 400 from the
// sign-in start can only be about the email: the app makes the other fields.
func serviceError(err error, badRequest ErrorCode) error {
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
		case apiErr.Status == http.StatusBadRequest && badRequest != "":
			code = badRequest
		case apiErr.Status == http.StatusTooManyRequests:
			code = ErrRateLimited
		case apiErr.Code == "email_unavailable":
			code = ErrEmailUnavailable
		}
	case errors.As(err, &urlErr):
		code = ErrOffline
	}
	return &Error{Code: code, err: err}
}
