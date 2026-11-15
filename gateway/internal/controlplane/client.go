// Package controlplane is the gateway's side of the control API
// (contracts/control-api.openapi.yaml, the /v1/gateway/* paths). Client
// speaks HTTP; Controller runs the gateway from the API: it applies the
// config, holds a quota lease for each peer's session, and reports usage
// and line events.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	requestTimeout  = 15 * time.Second
	maxConfigBody   = 16 << 20
	maxResponseBody = 1 << 20
)

// GatewaySelf is the body of PUT /v1/gateway/self.
type GatewaySelf struct {
	WGPublicKey    string `json:"wg_public_key"`
	WGEndpoint     string `json:"wg_endpoint"`
	QUICEndpoint   string `json:"quic_endpoint"`
	QUICServerName string `json:"quic_server_name"`
	QUICSPKISHA256 string `json:"quic_spki_sha256"` // lowercase hex
	Version        string `json:"version"`
}

// Config is the body of GET /v1/gateway/config.
type Config struct {
	ETag           string   `json:"etag"`
	TopUpHostnames []string `json:"topup_hostnames"`
	// FailbackSeconds is how long a peer's own line must be online without
	// a break before its session on a fallback line moves back to it.
	FailbackSeconds int          `json:"failback_seconds"`
	Peers           []ConfigPeer `json:"peers"`
	Lines           []ConfigLine `json:"lines"`
}

// ConfigPeer is a WireGuard peer the gateway serves.
type ConfigPeer struct {
	PeerID    string `json:"peer_id"`
	MemberID  string `json:"member_id"`
	PublicKey string `json:"public_key"`
	IPv4      string `json:"ipv4"`
	IPv6      string `json:"ipv6"`
	LineID    string `json:"line_id"` // the line the member chose
	// FallbackLineIDs take over while LineID is offline, best first.
	FallbackLineIDs []string `json:"fallback_line_ids"`
	// HTTPSOnly admits only TLS on TCP from the device.
	HTTPSOnly bool `json:"https_only"`
}

// hasLine reports whether lineID is the peer's own line or one of its
// fallback lines.
func (p ConfigPeer) hasLine(lineID string) bool {
	return lineID == p.LineID || slices.Contains(p.FallbackLineIDs, lineID)
}

// sameDevice reports whether a and b are the same WireGuard device with
// the same HTTPS only setting: the same key, addresses and member. Its
// lines may differ. A changed setting counts as another device, so the peer
// is added again: its session ends and its open flows are reset, and no
// unencrypted flow outlives turning HTTPS only on.
func sameDevice(a, b ConfigPeer) bool {
	return a.PeerID == b.PeerID && a.MemberID == b.MemberID && a.PublicKey == b.PublicKey && a.IPv4 == b.IPv4 && a.IPv6 == b.IPv6 &&
		a.HTTPSOnly == b.HTTPSOnly
}

// ConfigLine is a shared line whose agent may connect.
type ConfigLine struct {
	LineID           string `json:"line_id"`
	OwnerMemberID    string `json:"owner_member_id"`
	DeviceSPKISHA256 string `json:"device_spki_sha256"` // lowercase hex
}

// LeaseRequest is the body of POST /v1/gateway/leases.
type LeaseRequest struct {
	RequestID         string `json:"request_id"`
	PeerID            string `json:"peer_id"`
	SessionID         string `json:"session_id"`
	LineID            string `json:"line_id"` // the line the session exits through
	SupersedesLeaseID string `json:"supersedes_lease_id,omitempty"`
}

// Lease is a quota lease, counted in bytes.
type Lease struct {
	LeaseID      string    `json:"lease_id"`
	GrantedBytes int64     `json:"granted_bytes"`
	RenewAtBytes int64     `json:"renew_at_bytes"`
	ExpiresAt    time.Time `json:"expires_at"`
	GraceSeconds int       `json:"grace_seconds"`

	// ServerDate is the API's Date header on the answer, if it sent one.
	// expires_at is on the API's clock; ServerDate lets the gateway convert
	// it to its own.
	ServerDate time.Time `json:"-"`
}

// UsageEvent is the bytes one session used in one period.
type UsageEvent struct {
	EventID      string        `json:"event_id"`
	SessionID    string        `json:"session_id"`
	PeerID       string        `json:"peer_id"`
	LineID       string        `json:"line_id"`
	LeaseID      string        `json:"lease_id"`
	Seq          int           `json:"seq"`
	BytesUp      uint64        `json:"bytes_up"`
	BytesDown    uint64        `json:"bytes_down"`
	PeriodStart  string        `json:"period_start"`
	PeriodEnd    string        `json:"period_end"`
	Final        bool          `json:"final"`
	SessionStart *SessionStart `json:"session_start,omitempty"`
}

// SessionStart is on a session's first event only.
type SessionStart struct {
	StartedAt string `json:"started_at"`
	ExitIP    string `json:"exit_ip"`
}

// LineEvent tells the API that a line went online or offline.
type LineEvent struct {
	EventID      string `json:"event_id"`
	LineID       string `json:"line_id"`
	Type         string `json:"type"` // "online", "offline" or "tampered"
	ObservedIP   string `json:"observed_ip"`
	AgentVersion string `json:"agent_version"`
	At           string `json:"at"`
	Check        string `json:"check,omitempty"` // for "tampered": "http" or "dns"
}

// EventResult is the API's answer for one usage or line event.
type EventResult struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"` // applied, duplicate or rejected
	Reason  string `json:"reason,omitempty"`
}

// InsufficientCreditsError is a 402 answer to a lease request: the member
// has no credits for another lease.
type InsufficientCreditsError struct {
	GraceSeconds int
}

func (e *InsufficientCreditsError) Error() string {
	return fmt.Sprintf("controlplane: insufficient credits (grace %d s)", e.GraceSeconds)
}

// ErrPeerInactive is a 403 answer to a lease request: the peer may not be
// used any more, and its session closes at once.
var ErrPeerInactive = errors.New("controlplane: peer inactive")

// APIError is any other answer the API did not succeed with.
type APIError struct {
	Status int    // HTTP status; 0 when the API was not reached
	Code   string // the body's error code, if any
	Err    error  // the transport error when Status is 0
}

func (e *APIError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("controlplane: API unreachable: %v", e.Err)
	}
	if e.Code != "" {
		return fmt.Sprintf("controlplane: API answered %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("controlplane: API answered %d", e.Status)
}

func (e *APIError) Unwrap() error { return e.Err }

// Temporary reports whether retrying the same request may succeed: the API
// was unreachable, refused the token (401, until it is fixed), was
// overloaded (429) or failed (5xx). The API stores none of these answers,
// so a retry with the same request ID runs again.
func Temporary(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == 0 || ae.Status == http.StatusTooManyRequests || ae.Status == http.StatusUnauthorized || ae.Status >= 500
}

// Client calls the control API with a gateway token.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a client for the API at baseURL, authenticating with
// token (hpgw_<id>.<secret>).
func NewClient(baseURL, token string) *Client {
	return &Client{
		base:  strings.TrimSuffix(baseURL, "/"),
		token: token,
		http:  &http.Client{Timeout: requestTimeout},
	}
}

// PutSelf announces the gateway's endpoints and keys.
func (c *Client) PutSelf(ctx context.Context, self GatewaySelf) error {
	return c.do(ctx, http.MethodPut, "/v1/gateway/self", self, nil, nil)
}

// Config fetches the gateway's config. With etag set to the body's etag of
// the config the gateway has, it reports changed false when nothing changed.
func (c *Client) Config(ctx context.Context, etag string) (cfg Config, changed bool, err error) {
	h := http.Header{}
	if etag != "" {
		h.Set("If-None-Match", etag)
	}
	status, err := c.doStatus(ctx, http.MethodGet, "/v1/gateway/config", nil, h, &cfg, http.StatusNotModified)
	if err != nil {
		return Config{}, false, err
	}
	if status == http.StatusNotModified {
		return Config{}, false, nil
	}
	return cfg, true, nil
}

// CreateLease asks for a quota lease. A 402 answer is an
// *InsufficientCreditsError and a 403 answer is ErrPeerInactive.
func (c *Client) CreateLease(ctx context.Context, req LeaseRequest) (Lease, error) {
	var l Lease
	_, hdr, err := c.send(ctx, http.MethodPost, "/v1/gateway/leases", req, nil, &l)
	if err == nil {
		if d, perr := http.ParseTime(hdr.Get("Date")); perr == nil {
			l.ServerDate = d
		}
	}
	return l, err
}

// ReturnLease returns the unused part of a lease. A lease the API no
// longer knows (404) counts as returned.
func (c *Client) ReturnLease(ctx context.Context, leaseID, requestID string) error {
	body := struct {
		RequestID string `json:"request_id"`
	}{requestID}
	err := c.do(ctx, http.MethodPost, "/v1/gateway/leases/"+url.PathEscape(leaseID)+"/return", body, nil, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// ReportUsage sends up to 100 usage events and returns one result per event.
func (c *Client) ReportUsage(ctx context.Context, events []UsageEvent) ([]EventResult, error) {
	return c.report(ctx, "/v1/gateway/usage", struct {
		Events []UsageEvent `json:"events"`
	}{events}, len(events))
}

// ReportLineEvents sends up to 100 line events and returns one result per event.
func (c *Client) ReportLineEvents(ctx context.Context, events []LineEvent) ([]EventResult, error) {
	return c.report(ctx, "/v1/gateway/line-events", struct {
		Events []LineEvent `json:"events"`
	}{events}, len(events))
}

func (c *Client) report(ctx context.Context, path string, body any, n int) ([]EventResult, error) {
	var res struct {
		Results []EventResult `json:"results"`
	}
	if err := c.do(ctx, http.MethodPost, path, body, nil, &res); err != nil {
		return nil, err
	}
	if len(res.Results) != n {
		return nil, fmt.Errorf("controlplane: %d results for %d events", len(res.Results), n)
	}
	return res.Results, nil
}

func (c *Client) do(ctx context.Context, method, path string, in any, h http.Header, out any) error {
	_, _, err := c.send(ctx, method, path, in, h, out)
	return err
}

func (c *Client) doStatus(ctx context.Context, method, path string, in any, h http.Header, out any, also ...int) (int, error) {
	status, _, err := c.send(ctx, method, path, in, h, out, also...)
	return status, err
}

// send sends one request. A 2xx answer is decoded into out; a status in
// also is returned without error and without a body.
func (c *Client) send(ctx context.Context, method, path string, in any, h http.Header, out any, also ...int) (int, http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, nil, fmt.Errorf("controlplane: encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("controlplane: %w", err)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, &APIError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	limit := int64(maxResponseBody)
	if path == "/v1/gateway/config" {
		limit = maxConfigBody
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return 0, nil, &APIError{Err: err}
	}
	for _, s := range also {
		if resp.StatusCode == s {
			return s, resp.Header, nil
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, resp.Header, errorFor(resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, resp.Header, fmt.Errorf("controlplane: decode %s answer: %w", path, err)
		}
	}
	return resp.StatusCode, resp.Header, nil
}

func errorFor(status int, body []byte) error {
	var e struct {
		Error        string `json:"error"`
		GraceSeconds *int   `json:"grace_seconds"`
	}
	_ = json.Unmarshal(body, &e)
	switch {
	case status == http.StatusPaymentRequired && e.Error == "insufficient_credits" && e.GraceSeconds != nil:
		return &InsufficientCreditsError{GraceSeconds: *e.GraceSeconds}
	case status == http.StatusForbidden && e.Error == "peer_inactive":
		return ErrPeerInactive
	}
	return &APIError{Status: status, Code: e.Error}
}
