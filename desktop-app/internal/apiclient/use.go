package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// CreateIdentitySession starts a Stripe Identity check and returns the URL
// of its page. A member who is verified already gets an *Error with status
// 409 and code identity_already_verified.
func (c *Client) CreateIdentitySession(ctx context.Context, sessionToken, requestID string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	body := map[string]string{"request_id": requestID}
	err := c.do(ctx, http.MethodPost, "/v1/identity/sessions", sessionToken, body, &out, http.StatusCreated)
	return out.URL, err
}

// CreateCheckoutSession starts a Stripe Checkout for one credit pack and
// returns the URL of its page. It needs a verified identity (403
// identity_required).
func (c *Client) CreateCheckoutSession(ctx context.Context, sessionToken, requestID string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	body := map[string]string{"request_id": requestID}
	err := c.do(ctx, http.MethodPost, "/v1/checkout/sessions", sessionToken, body, &out, http.StatusCreated)
	return out.URL, err
}

// AvailableLine is a line the member can use. City and ISP are empty when
// the API doesn't know them.
type AvailableLine struct {
	ID      string  `json:"line_id"`
	Country string  `json:"country"`
	City    *string `json:"city"`
	ISP     *string `json:"isp"`
	IPv6    bool    `json:"ipv6"`
	Online  bool    `json:"online"`
}

// AvailableLines lists the lines the member can use: lines that are
// sharing, minus the member's own and lines linked to them. It needs a
// verified identity (403 identity_required).
func (c *Client) AvailableLines(ctx context.Context, sessionToken string) ([]AvailableLine, error) {
	var out struct {
		Lines []AvailableLine `json:"lines"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/lines/available", sessionToken, nil, &out, http.StatusOK)
	return out.Lines, err
}

// Peer is one of the member's WireGuard peers: a device that uses a line.
type Peer struct {
	ID        string    `json:"peer_id"`
	Name      string    `json:"name"`
	LineID    string    `json:"line_id"`
	PublicKey string    `json:"public_key"`
	IPv4      string    `json:"ipv4"`
	IPv6      string    `json:"ipv6"`
	CreatedAt time.Time `json:"created_at"`
}

// PeerCreated is a new peer and its .conf file. ConfigText has
// {{PRIVATE_KEY}} where the client puts the device's private key.
type PeerCreated struct {
	Peer       Peer   `json:"peer"`
	ConfigText string `json:"config_text"`
}

// CreatePeer creates a WireGuard peer on lineID for the device whose public
// key (standard base64) is given. The private key never leaves the app.
func (c *Client) CreatePeer(ctx context.Context, sessionToken, requestID, lineID, name, publicKey string) (PeerCreated, error) {
	body := map[string]string{
		"request_id": requestID,
		"line_id":    lineID,
		"name":       name,
		"public_key": publicKey,
	}
	var out PeerCreated
	err := c.do(ctx, http.MethodPost, "/v1/wireguard/peers", sessionToken, body, &out, http.StatusCreated)
	return out, err
}

// Peers lists the member's WireGuard peers, newest first.
func (c *Client) Peers(ctx context.Context, sessionToken string) ([]Peer, error) {
	var out struct {
		Peers []Peer `json:"peers"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/wireguard/peers", sessionToken, nil, &out, http.StatusOK)
	return out.Peers, err
}

// DeletePeer deletes a WireGuard peer. A peer that doesn't exist, or was
// deleted already, is an *Error with status 404.
func (c *Client) DeletePeer(ctx context.Context, sessionToken, peerID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/wireguard/peers/"+url.PathEscape(peerID), sessionToken, nil, nil, http.StatusNoContent)
}
