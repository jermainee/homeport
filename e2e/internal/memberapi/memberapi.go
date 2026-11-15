// Package memberapi is a small client for the control API's member
// endpoints (contracts/control-api.openapi.yaml) and its dev-only routes,
// for the full-stack smoke test and the dev tools in e2e/cmd. It stands in
// for the desktop app's Use mode until that exists.
package memberapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
)

// Client calls the API as one member, or as no one before sign-in.
type Client struct {
	BaseURL string // such as http://localhost:8787
	// Token is the member's session token (hpm_...). StartLogin activates
	// it; dev routes don't need it.
	Token string
	HTTP  *http.Client // nil means a client with a 30 s timeout
}

// Error is an answer other than the one the call expects.
type Error struct {
	Method, Path string
	Status       int
	Code         string // the body's error, such as line_not_allowed
	Message      string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, e.Code)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// NewSessionToken returns a session token as the app creates one: hpm_ and
// 32 random bytes in base64url.
func NewSessionToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return "hpm_" + base64.RawURLEncoding.EncodeToString(b)
}

// NewRequestID returns a lowercase UUIDv7, the request_id every member POST
// carries.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[6:])
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixMilli())) // #nosec G115 -- times are after 1970
	copy(b[:6], ts[2:])
	b[6] = 0x70 | b[6]&0x0f // version 7
	b[8] = 0x80 | b[8]&0x3f // RFC 9562 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// LoginRequest is a started sign-in.
type LoginRequest struct {
	ID        string    `json:"login_request_id"`
	MatchCode string    `json:"match_code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// StartLogin asks for a sign-in link for email, for c.Token. The API writes
// the link to its log in dev (EMAIL_MODE=log).
func (c *Client) StartLogin(ctx context.Context, email string) (LoginRequest, error) {
	sum := sha256.Sum256([]byte(c.Token))
	var lr LoginRequest
	err := c.do(ctx, http.MethodPost, "/v1/auth/login-requests", false, map[string]any{
		"request_id":           NewRequestID(),
		"email":                email,
		"session_token_sha256": hex.EncodeToString(sum[:]),
	}, http.StatusCreated, &lr)
	return lr, err
}

// DevConfirm approves a login request without the email link (dev only).
func (c *Client) DevConfirm(ctx context.Context, loginRequestID string) error {
	return c.do(ctx, http.MethodPost, "/dev/login-requests/"+url.PathEscape(loginRequestID)+"/confirm", false,
		map[string]any{"session_token": c.Token}, http.StatusOK, nil)
}

// Approved is a sign-in the member approved: whose session it is, and
// when the session ends.
type Approved struct {
	MemberID  string    `json:"member_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// WaitLogin polls a login request every interval until the member
// approved it.
func (c *Client) WaitLogin(ctx context.Context, loginRequestID string, interval time.Duration) (Approved, error) {
	for {
		var answer struct {
			Approved
			Status string `json:"status"`
		}
		status, err := c.call(ctx, http.MethodGet, "/v1/auth/login-requests/"+url.PathEscape(loginRequestID), true, nil, &answer)
		if err != nil {
			return Approved{}, err
		}
		if status == http.StatusOK && answer.Status == "approved" {
			return answer.Approved, nil
		}
		select {
		case <-ctx.Done():
			return Approved{}, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// SignInDev signs in as email through the dev confirm route, with a new
// session token, and returns the member's id.
func (c *Client) SignInDev(ctx context.Context, email string) (string, error) {
	c.Token = NewSessionToken()
	lr, err := c.StartLogin(ctx, email)
	if err != nil {
		return "", err
	}
	if err := c.DevConfirm(ctx, lr.ID); err != nil {
		return "", err
	}
	approved, err := c.WaitLogin(ctx, lr.ID, 100*time.Millisecond)
	return approved.MemberID, err
}

// Credits are a member's ledger balances.
type Credits struct {
	Purchased      int64 `json:"purchased"`
	EarnedPending  int64 `json:"earned_pending"`
	EarnedCashable int64 `json:"earned_cashable"`
}

// Me is the signed-in member.
type Me struct {
	MemberID       string  `json:"member_id"`
	Email          string  `json:"email"`
	IdentityStatus string  `json:"identity_status"`
	Credits        Credits `json:"credits"`
}

// Me returns the signed-in member.
func (c *Client) Me(ctx context.Context) (Me, error) {
	var me Me
	err := c.do(ctx, http.MethodGet, "/v1/me", true, nil, http.StatusOK, &me)
	return me, err
}

// StartIdentityCheck starts a Stripe Identity check and returns the page
// the member opens.
func (c *Client) StartIdentityCheck(ctx context.Context) (string, error) {
	var s struct {
		URL string `json:"url"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/identity/sessions", true, map[string]any{"request_id": NewRequestID()}, http.StatusCreated, &s)
	return s.URL, err
}

// StartCheckout starts buying a credit pack and returns the Stripe Checkout
// page the member opens.
func (c *Client) StartCheckout(ctx context.Context) (string, error) {
	var s struct {
		URL string `json:"url"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/checkout/sessions", true, map[string]any{"request_id": NewRequestID()}, http.StatusCreated, &s)
	return s.URL, err
}

// Device is a registered sharing agent.
type Device struct {
	ID         string `json:"device_id"`
	SPKISHA256 string `json:"spki_sha256"`
}

// RegisterDevice registers a sharing agent's key, or returns the device
// the member registered it as before.
func (c *Client) RegisterDevice(ctx context.Context, name, platform string, agentKey ed25519.PublicKey) (Device, error) {
	var d Device
	_, err := c.call(ctx, http.MethodPost, "/v1/devices", true, map[string]any{
		"request_id":       NewRequestID(),
		"name":             name,
		"platform":         platform,
		"agent_public_key": base64.StdEncoding.EncodeToString(agentKey),
	}, &d)
	return d, err
}

// Platform is this machine's platform as the API names it: linux, macos or
// windows.
func Platform() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

// Line is a device's line and the gateway its agent dials.
type Line struct {
	ID      string `json:"line_id"`
	Gateway struct {
		QUICEndpoint string `json:"quic_endpoint"`
		ServerName   string `json:"server_name"`
		SPKISHA256   string `json:"spki_sha256"`
	} `json:"gateway"`
}

// CreateLine creates the device's line, with sharing off, or returns the
// line it has.
func (c *Client) CreateLine(ctx context.Context, deviceID string) (Line, error) {
	var l Line
	_, err := c.call(ctx, http.MethodPost, "/v1/lines", true, map[string]any{
		"request_id": NewRequestID(),
		"device_id":  deviceID,
	}, &l)
	return l, err
}

// SetSharing turns sharing on the line on or off.
func (c *Client) SetSharing(ctx context.Context, lineID string, on bool) error {
	return c.do(ctx, http.MethodPatch, "/v1/lines/"+url.PathEscape(lineID), true,
		map[string]any{"sharing_enabled": on}, http.StatusOK, nil)
}

// AvailableLine is a line the member may use.
type AvailableLine struct {
	ID      string `json:"line_id"`
	Country string `json:"country"`
	City    string `json:"city"`
	ISP     string `json:"isp"`
	Online  bool   `json:"online"`
}

// AvailableLines lists the lines the member may use.
func (c *Client) AvailableLines(ctx context.Context) ([]AvailableLine, error) {
	var l struct {
		Lines []AvailableLine `json:"lines"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/lines/available", true, nil, http.StatusOK, &l)
	return l.Lines, err
}

// Peer is a created WireGuard peer.
type Peer struct {
	Peer struct {
		ID        string `json:"peer_id"`
		IPv4      string `json:"ipv4"`
		IPv6      string `json:"ipv6"`
		HTTPSOnly bool   `json:"https_only"`
	} `json:"peer"`
	// ConfigText is the .conf file with {{PRIVATE_KEY}} where the client's
	// key goes (wgclient.WithPrivateKey).
	ConfigText string `json:"config_text"`
}

// CreatePeer adds a WireGuard peer with publicKey (base64) on a line.
func (c *Client) CreatePeer(ctx context.Context, lineID, name, publicKey string) (Peer, error) {
	var p Peer
	err := c.do(ctx, http.MethodPost, "/v1/wireguard/peers", true, map[string]any{
		"request_id": NewRequestID(),
		"line_id":    lineID,
		"name":       name,
		"public_key": publicKey,
	}, http.StatusCreated, &p)
	return p, err
}

// SetHTTPSOnly turns a peer's HTTPS only on or off.
func (c *Client) SetHTTPSOnly(ctx context.Context, peerID string, on bool) error {
	return c.do(ctx, http.MethodPatch, "/v1/wireguard/peers/"+url.PathEscape(peerID), true,
		map[string]any{"https_only": on}, http.StatusOK, nil)
}

// DevVerifyIdentity marks a member's identity verified (dev only).
func (c *Client) DevVerifyIdentity(ctx context.Context, memberID string) error {
	return c.do(ctx, http.MethodPost, "/dev/members/"+url.PathEscape(memberID)+"/identity/verify", false, nil, http.StatusOK, nil)
}

// DevGrantCredits grants a member purchased credits (dev only).
func (c *Client) DevGrantCredits(ctx context.Context, memberID string, credits int64) error {
	return c.do(ctx, http.MethodPost, "/dev/members/"+url.PathEscape(memberID)+"/credits", false,
		map[string]any{"credits": credits}, http.StatusOK, nil)
}

// do sends a request and decodes the answer into out, or returns an *Error
// unless the status is want.
func (c *Client) do(ctx context.Context, method, path string, auth bool, body any, want int, out any) error {
	status, err := c.call(ctx, method, path, auth, body, out)
	if err == nil && status != want {
		err = &Error{Method: method, Path: path, Status: status, Code: "unexpected status"}
	}
	return err
}

// call sends a request. A 2xx answer is decoded into out; any other is an
// *Error.
func (c *Client) call(ctx context.Context, method, path string, auth bool, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		if c.Token == "" {
			return 0, errors.New("memberapi: not signed in")
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, &Error{Method: method, Path: path, Status: resp.StatusCode, Code: e.Error, Message: e.Message}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}
