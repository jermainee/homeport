// Package apiclient calls the member and share endpoints of the Homeport
// control API, as contracts/control-api.openapi.yaml describes them.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the API that `wrangler dev` serves in api/.
const DefaultBaseURL = "http://localhost:8787"

// BaseURL returns the API base URL: HOMEPORT_API, or DefaultBaseURL.
func BaseURL(getenv func(string) string) string {
	if u := strings.TrimRight(getenv("HOMEPORT_API"), "/"); u != "" {
		return u
	}
	return DefaultBaseURL
}

// Client calls the control API. It is safe for concurrent use.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a client for the API at baseURL. A nil httpClient uses one
// with a 15-second timeout.
func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}
}

// Error is an error response from the API: `{error, message?}` and its
// HTTP status.
type Error struct {
	Status  int
	Code    string
	Message string
	// ServerTime is the API's clock from the response's Date header, or
	// zero without one.
	ServerTime time.Time
	// RetryAfter is how long the API asks the client to wait, from the
	// Retry-After header (seconds or an HTTP date), or zero.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("api: %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("api: %d %s", e.Status, e.Code)
}

// IsStatus reports whether err is an API error with the given HTTP status.
func IsStatus(err error, status int) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// LoginRequest is a started sign-in.
type LoginRequest struct {
	ID        string    `json:"login_request_id"`
	MatchCode string    `json:"match_code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreateLoginRequest starts an email sign-in. requestID is a UUIDv7 that
// stays the same on every retry of this sign-in; sessionTokenSHA256 is the
// lowercase hex SHA-256 of the session token the app created.
func (c *Client) CreateLoginRequest(ctx context.Context, requestID, email, sessionTokenSHA256 string) (LoginRequest, error) {
	body := map[string]string{
		"request_id":           requestID,
		"email":                email,
		"session_token_sha256": sessionTokenSHA256,
	}
	var out LoginRequest
	err := c.do(ctx, http.MethodPost, "/v1/auth/login-requests", "", body, &out, http.StatusCreated)
	return out, err
}

// do sends a request and decodes a JSON response with one of the ok
// statuses into out. Any other status becomes an *Error.
func (c *Client) do(ctx context.Context, method, path, token string, body, out any, ok ...int) error {
	_, err := c.doStatus(ctx, method, path, token, body, out, ok...)
	return err
}

func (c *Client) doStatus(ctx context.Context, method, path, token string, body, out any, ok ...int) (int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	for _, s := range ok {
		if resp.StatusCode != s {
			continue
		}
		if out == nil || resp.StatusCode == http.StatusNoContent {
			return resp.StatusCode, nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("api: decode %s %s: %w", method, path, err)
		}
		return resp.StatusCode, nil
	}
	apiErr := &Error{Status: resp.StatusCode}
	if t, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		apiErr.ServerTime = t
	}
	apiErr.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), apiErr.ServerTime)
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &payload) == nil && payload.Error != "" {
		apiErr.Code, apiErr.Message = payload.Error, payload.Message
	} else {
		apiErr.Code = strings.ReplaceAll(strings.ToLower(http.StatusText(resp.StatusCode)), " ", "_")
	}
	return resp.StatusCode, apiErr
}

// retryAfter reads a Retry-After value: delay seconds, or an HTTP date
// measured against the API's clock (now when it sent no Date).
func retryAfter(v string, serverTime time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return max(0, time.Duration(secs)*time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		if serverTime.IsZero() {
			serverTime = time.Now()
		}
		return max(0, t.Sub(serverTime))
	}
	return 0
}

// LoginStatus is the state of a sign-in: pending until the member approves
// it, then approved.
type LoginStatus struct {
	Approved  bool
	MemberID  string
	ExpiresAt time.Time // when the session token stops working
}

// GetLoginRequest polls a sign-in with the session token the app created
// for it. A sign-in that expired unapproved is an *Error with status 410.
func (c *Client) GetLoginRequest(ctx context.Context, loginRequestID, sessionToken string) (LoginStatus, error) {
	var out struct {
		Status    string    `json:"status"`
		MemberID  string    `json:"member_id"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := "/v1/auth/login-requests/" + url.PathEscape(loginRequestID)
	status, err := c.doStatus(ctx, http.MethodGet, path, sessionToken, nil, &out, http.StatusOK, http.StatusAccepted)
	if err != nil {
		return LoginStatus{}, err
	}
	if status == http.StatusAccepted {
		return LoginStatus{}, nil
	}
	return LoginStatus{Approved: true, MemberID: out.MemberID, ExpiresAt: out.ExpiresAt}, nil
}

// Member is the signed-in member.
type Member struct {
	MemberID       string        `json:"member_id"`
	Email          string        `json:"email"`
	IdentityStatus string        `json:"identity_status"`
	Credits        CreditBalance `json:"credits"`
}

// CreditBalance is the member's credits by kind.
type CreditBalance struct {
	Purchased      int64 `json:"purchased"`
	EarnedPending  int64 `json:"earned_pending"`
	EarnedCashable int64 `json:"earned_cashable"`
}

// Me returns the member the session token belongs to.
func (c *Client) Me(ctx context.Context, sessionToken string) (Member, error) {
	var out Member
	err := c.do(ctx, http.MethodGet, "/v1/me", sessionToken, nil, &out, http.StatusOK)
	return out, err
}

// DeleteSession signs out: the session token stops working. A token that
// already doesn't work is an *Error with status 401.
func (c *Client) DeleteSession(ctx context.Context, sessionToken string) error {
	return c.do(ctx, http.MethodDelete, "/v1/auth/session", sessionToken, nil, nil, http.StatusNoContent)
}

// Device is a registered sharing agent key.
type Device struct {
	ID         string `json:"device_id"`
	SPKISHA256 string `json:"spki_sha256"`
}

// RegisterDevice registers the sharing agent's Ed25519 key, given as raw
// standard base64. platform is linux, macos or windows. Registering a key the
// member registered before returns that device.
func (c *Client) RegisterDevice(ctx context.Context, sessionToken, requestID, name, platform, agentPublicKey string) (Device, error) {
	body := map[string]string{
		"request_id":       requestID,
		"name":             name,
		"platform":         platform,
		"agent_public_key": agentPublicKey,
	}
	var out Device
	err := c.do(ctx, http.MethodPost, "/v1/devices", sessionToken, body, &out, http.StatusCreated, http.StatusOK)
	return out, err
}

// AgentGateway is where a line's sharing agent dials out to. SPKISHA256 is
// the lowercase hex SHA-256 of the gateway certificate's SPKI, which the
// agent pins.
type AgentGateway struct {
	QUICEndpoint string `json:"quic_endpoint"`
	ServerName   string `json:"server_name"`
	SPKISHA256   string `json:"spki_sha256"`
}

// LineCreated is a device's line and its gateway.
type LineCreated struct {
	ID      string       `json:"line_id"`
	Gateway AgentGateway `json:"gateway"`
}

// CreateLine creates the line deviceID shares, or returns the line it has.
// It needs a verified identity (403 identity_required).
func (c *Client) CreateLine(ctx context.Context, sessionToken, requestID, deviceID string) (LineCreated, error) {
	body := map[string]string{"request_id": requestID, "device_id": deviceID}
	var out LineCreated
	err := c.do(ctx, http.MethodPost, "/v1/lines", sessionToken, body, &out, http.StatusCreated, http.StatusOK)
	return out, err
}

// UpdateLine turns sharing on lineID on or off.
func (c *Client) UpdateLine(ctx context.Context, sessionToken, lineID string, sharingEnabled bool) error {
	body := map[string]bool{"sharing_enabled": sharingEnabled}
	return c.do(ctx, http.MethodPatch, "/v1/lines/"+url.PathEscape(lineID), sessionToken, body, nil, http.StatusOK)
}
