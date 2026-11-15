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

// Region is a country, or a city, with how many free lines the member can
// get there. Cities lists a country's cities when the request named it.
type Region struct {
	Country string   `json:"country"`
	City    string   `json:"city"`
	Lines   int      `json:"lines"`
	Cities  []Region `json:"cities"`
}

// Regions lists the countries with free lines for the member, or with
// country, that country and its cities, most lines first. It needs a
// verified identity (403 identity_required).
func (c *Client) Regions(ctx context.Context, sessionToken, country string) ([]Region, error) {
	path := "/v1/lines/regions"
	if country != "" {
		path += "?country=" + url.QueryEscape(country)
	}
	var out struct {
		Countries []Region `json:"countries"`
	}
	err := c.do(ctx, http.MethodGet, path, sessionToken, nil, &out, http.StatusOK)
	return out.Countries, err
}

// Peer is one of the member's WireGuard peers: a device that uses a line.
type Peer struct {
	ID     string `json:"peer_id"`
	Name   string `json:"name"`
	LineID string `json:"line_id"` // the line the member chose
	// Line is where LineID is.
	Line LinePlace `json:"line"`
	// Failover is where the peer goes while its line is offline: country,
	// city or off.
	Failover string `json:"failover"`
	// ExitLine is the line the peer's current session exits through: LineID,
	// or a backup line while LineID is offline. Nil without a session.
	ExitLine  *ExitLine `json:"exit_line"`
	PublicKey string    `json:"public_key"`
	IPv4      string    `json:"ipv4"`
	IPv6      string    `json:"ipv6"`
	CreatedAt time.Time `json:"created_at"`
}

// LinePlace is where a peer's line is. Each part is nil when the API
// doesn't know it.
type LinePlace struct {
	Country *string `json:"country"`
	City    *string `json:"city"`
	ISP     *string `json:"isp"`
}

// ExitLine is the line a peer's session exits through, as the gateway last
// reported it. Country and City are nil when the API doesn't know them.
type ExitLine struct {
	LineID  string    `json:"line_id"`
	Country *string   `json:"country"`
	City    *string   `json:"city"`
	Since   time.Time `json:"since"` // when the session on this line started
}

// PeerCreated is a new peer and its .conf file. ConfigText has
// {{PRIVATE_KEY}} where the client puts the device's private key.
type PeerCreated struct {
	Peer       Peer   `json:"peer"`
	ConfigText string `json:"config_text"`
}

// CreatePeer creates a WireGuard peer for the device whose public key
// (standard base64) is given, on a free line the API picks in country, and
// in city unless it is empty. The private key never leaves the app.
func (c *Client) CreatePeer(ctx context.Context, sessionToken, requestID, country, city, name, publicKey string) (PeerCreated, error) {
	body := map[string]string{
		"request_id": requestID,
		"country":    country,
		"name":       name,
		"public_key": publicKey,
	}
	if city != "" {
		body["city"] = city
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

// UpdatePeer moves a WireGuard peer to a free line the API picks in
// country (and city unless it is empty), or changes its failover, or both;
// an empty argument keeps what the peer has. The peer keeps its .conf
// file. A peer that doesn't exist is an *Error with status 404; a region
// without another free line on the peer's gateway is 409 no_free_line.
func (c *Client) UpdatePeer(ctx context.Context, sessionToken, peerID, country, city, failover string) (Peer, error) {
	body := map[string]string{}
	if country != "" {
		body["country"] = country
		if city != "" {
			body["city"] = city
		}
	}
	if failover != "" {
		body["failover"] = failover
	}
	var out Peer
	err := c.do(ctx, http.MethodPatch, "/v1/wireguard/peers/"+url.PathEscape(peerID), sessionToken, body, &out, http.StatusOK)
	return out, err
}

// DeletePeer deletes a WireGuard peer. A peer that doesn't exist, or was
// deleted already, is an *Error with status 404.
func (c *Client) DeletePeer(ctx context.Context, sessionToken, peerID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/wireguard/peers/"+url.PathEscape(peerID), sessionToken, nil, nil, http.StatusNoContent)
}
