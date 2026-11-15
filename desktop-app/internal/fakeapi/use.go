package fakeapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"
)

// AvailableLine is a line another member shares, as GET /v1/lines/available
// lists it.
type AvailableLine struct {
	LineID  string  `json:"line_id"`
	Country string  `json:"country"`
	City    *string `json:"city"`
	ISP     *string `json:"isp"`
	IPv6    bool    `json:"ipv6"`
	Online  bool    `json:"online"`
}

// Peer is a WireGuard peer as the API stores it. The API never sees the
// private key.
type Peer struct {
	PeerID    string    `json:"peer_id"`
	Name      string    `json:"name"`
	LineID    string    `json:"line_id"`
	Failover  string    `json:"failover"`
	ExitLine  *ExitLine `json:"exit_line"`
	PublicKey string    `json:"public_key"`
	IPv4      string    `json:"ipv4"`
	IPv6      string    `json:"ipv6"`
	CreatedAt time.Time `json:"created_at"`

	memberID string
	deleted  bool
}

// ExitLine is the line a peer's session exits through, as the gateway last
// reported it: the peer's line, or a backup line while that is offline.
type ExitLine struct {
	LineID  string    `json:"line_id"`
	Country *string   `json:"country"`
	City    *string   `json:"city"`
	Since   time.Time `json:"since"`
}

// SetExitLine sets the line peerID's session exits through, as the gateway
// reports it; nil means the peer has no session.
func (s *Server) SetExitLine(t testing.TB, peerID string, exit *ExitLine) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.PeerID == peerID && !p.deleted {
			p.ExitLine = exit
			return
		}
	}
	t.Fatalf("fakeapi: no peer %s", peerID)
}

// GatewayWireGuard is the gateway's WireGuard side, which every config
// names: its public key and endpoint.
const (
	GatewayWGPublicKey = "YND9Jx2S347AP/Ywks3VrMciTOYivx8pQxgWoPy1HHY="
	GatewayWGEndpoint  = "fra1.gw.homeport.test:51820"
)

// IdentityURL and CheckoutURL start the Stripe-hosted pages the fake
// returns; each session adds its number.
const (
	IdentityURL = "https://verify.stripe.test/start/"
	CheckoutURL = "https://checkout.stripe.test/c/pay/"
)

// OfferLine makes line available to every member but its owner, as a
// member who turns Share on does. It returns the line's id.
func (s *Server) OfferLine(line AvailableLine) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if line.LineID == "" {
		line.LineID = uuid.NewV7().String()
	}
	s.offered = append(s.offered, line)
	return line.LineID
}

// WithdrawLine stops offering a line, as when its owner turns Share off.
func (s *Server) WithdrawLine(lineID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offered = slices.DeleteFunc(s.offered, func(l AvailableLine) bool { return l.LineID == lineID })
}

// IdentitySessions returns the URLs of the identity checks email started,
// oldest first.
func (s *Server) IdentitySessions(t testing.TB, email string) []string {
	t.Helper()
	m := s.mustMember(t, email)
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.identityURLs[m.MemberID])
}

// CheckoutSessions returns the URLs of the checkouts email started, oldest
// first.
func (s *Server) CheckoutSessions(t testing.TB, email string) []string {
	t.Helper()
	m := s.mustMember(t, email)
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.checkoutURLs[m.MemberID])
}

// GrantPurchased adds purchased credits to email's balance, as the
// checkout.session.completed webhook does.
func (s *Server) GrantPurchased(t testing.TB, email string, credits int64) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.members[email]
	if !ok {
		t.Fatalf("fakeapi: no member %s", email)
	}
	m.Credits.Purchased += credits
}

// SetCredits sets email's balance.
func (s *Server) SetCredits(t testing.TB, email string, c Credits) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.members[email]
	if !ok {
		t.Fatalf("fakeapi: no member %s", email)
	}
	m.Credits = c
}

// Peers returns email's active peers, oldest first.
func (s *Server) Peers(t testing.TB, email string) []Peer {
	t.Helper()
	m := s.mustMember(t, email)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Peer
	for _, p := range s.peers {
		if p.memberID == m.MemberID && !p.deleted {
			out = append(out, *p)
		}
	}
	return out
}

func (s *Server) mustMember(t testing.TB, email string) Member {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.members[email]
	if !ok {
		t.Fatalf("fakeapi: no member %s", email)
	}
	return *m
}

func (s *Server) createIdentitySession(w http.ResponseWriter, r *http.Request) {
	m := s.memberRef(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		RequestID string `json:"request_id"`
	}
	raw, ok := decodeStrict(w, r, &body)
	if !ok || !uuidV7.MatchString(body.RequestID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayLocked(w, m, body.RequestID, raw) {
		return
	}
	if m.IdentityStatus == "verified" {
		writeError(w, http.StatusConflict, "identity_already_verified")
		return
	}
	s.sessionCount++
	url := fmt.Sprintf("%s%d", IdentityURL, s.sessionCount)
	s.identityURLs[m.MemberID] = append(s.identityURLs[m.MemberID], url)
	s.answerLocked(w, m, body.RequestID, raw, http.StatusCreated, map[string]string{"url": url})
}

func (s *Server) createCheckoutSession(w http.ResponseWriter, r *http.Request) {
	m := s.memberRef(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.verified(m) {
		writeError(w, http.StatusForbidden, "identity_required")
		return
	}
	var body struct {
		RequestID string `json:"request_id"`
	}
	raw, ok := decodeStrict(w, r, &body)
	if !ok || !uuidV7.MatchString(body.RequestID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayLocked(w, m, body.RequestID, raw) {
		return
	}
	s.sessionCount++
	url := fmt.Sprintf("%s%d", CheckoutURL, s.sessionCount)
	s.checkoutURLs[m.MemberID] = append(s.checkoutURLs[m.MemberID], url)
	s.answerLocked(w, m, body.RequestID, raw, http.StatusCreated, map[string]string{"url": url})
}

func (s *Server) listAvailableLines(w http.ResponseWriter, r *http.Request) {
	m := s.memberRef(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.verified(m) {
		writeError(w, http.StatusForbidden, "identity_required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := slices.Clone(s.offered)
	if lines == nil {
		lines = []AvailableLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (s *Server) createPeer(w http.ResponseWriter, r *http.Request) {
	m := s.memberRef(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.verified(m) {
		writeError(w, http.StatusForbidden, "identity_required")
		return
	}
	var body struct {
		RequestID string `json:"request_id"`
		LineID    string `json:"line_id"`
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	raw, ok := decodeStrict(w, r, &body)
	if !ok || !uuidV7.MatchString(body.RequestID) || body.LineID == "" ||
		len([]rune(body.Name)) < 1 || len([]rune(body.Name)) > 64 ||
		!key32.MatchString(body.PublicKey) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayLocked(w, m, body.RequestID, raw) {
		return
	}
	if !slices.ContainsFunc(s.offered, func(l AvailableLine) bool { return l.LineID == body.LineID }) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	for _, p := range s.peers {
		if !p.deleted && p.PublicKey == body.PublicKey {
			writeError(w, http.StatusConflict, "public_key_taken")
			return
		}
	}
	host := len(s.peers) + 2
	p := &Peer{
		PeerID:    uuid.NewV7().String(),
		Name:      body.Name,
		LineID:    body.LineID,
		Failover:  "country",
		PublicKey: body.PublicKey,
		IPv4:      fmt.Sprintf("10.77.%d.%d", host/256, host%256),
		IPv6:      fmt.Sprintf("fd77::%x", host),
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		memberID:  m.MemberID,
	}
	s.peers = append(s.peers, p)
	s.answerLocked(w, m, body.RequestID, raw, http.StatusCreated, map[string]any{
		"peer":        p,
		"config_text": peerConfig(p),
	})
}

// peerConfig is the .conf file the API returns, with the placeholder
// where the client puts its private key.
func peerConfig(p *Peer) string {
	return strings.Join([]string{
		"[Interface]",
		"PrivateKey = {{PRIVATE_KEY}}",
		fmt.Sprintf("Address = %s/32, %s/128", p.IPv4, p.IPv6),
		"DNS = 10.77.0.1",
		"MTU = 1280",
		"",
		"[Peer]",
		"PublicKey = " + GatewayWGPublicKey,
		"AllowedIPs = 0.0.0.0/0, ::/0",
		"Endpoint = " + GatewayWGEndpoint,
		"PersistentKeepalive = 25",
		"",
	}, "\n")
}

func (s *Server) listPeers(w http.ResponseWriter, r *http.Request) {
	m := s.memberRef(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*Peer{}
	for i := len(s.peers) - 1; i >= 0; i-- { // newest first
		if p := s.peers[i]; p.memberID == m.MemberID && !p.deleted {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": out})
}

// updatePeer moves a peer to another line, or changes its failover. Unlike
// the API, it knows only one gateway, so it never answers
// line_on_other_gateway or line_full; FailNext stands in for those.
func (s *Server) updatePeer(w http.ResponseWriter, r *http.Request) {
	m := s.memberRef(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.verified(m) {
		writeError(w, http.StatusForbidden, "identity_required")
		return
	}
	var body struct {
		LineID   *string `json:"line_id"`
		Failover *string `json:"failover"`
	}
	if _, ok := decodeStrict(w, r, &body); !ok ||
		(body.LineID == nil && body.Failover == nil) ||
		(body.LineID != nil && *body.LineID == "") ||
		(body.Failover != nil && !slices.Contains([]string{"country", "city", "off"}, *body.Failover)) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.peers, func(p *Peer) bool {
		return p.PeerID == r.PathValue("id") && p.memberID == m.MemberID && !p.deleted
	})
	if i < 0 {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	p := s.peers[i]
	if body.LineID != nil && *body.LineID != p.LineID &&
		!slices.ContainsFunc(s.offered, func(l AvailableLine) bool { return l.LineID == *body.LineID }) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if body.LineID != nil {
		p.LineID = *body.LineID
	}
	if body.Failover != nil {
		p.Failover = *body.Failover
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) deletePeer(w http.ResponseWriter, r *http.Request) {
	m := s.memberRef(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.PeerID == r.PathValue("id") && p.memberID == m.MemberID && !p.deleted {
			p.deleted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found")
}
