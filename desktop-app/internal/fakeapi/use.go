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

// AvailableLine is a line another member shares. GET /v1/lines/regions
// counts the online ones, and POST /v1/wireguard/peers picks one.
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
	Line      LinePlace `json:"line"`
	Failover  string    `json:"failover"`
	HTTPSOnly bool      `json:"https_only"`
	ExitLine  *ExitLine `json:"exit_line"`
	PublicKey string    `json:"public_key"`
	IPv4      string    `json:"ipv4"`
	IPv6      string    `json:"ipv6"`
	CreatedAt time.Time `json:"created_at"`

	memberID string
	deleted  bool
}

// LinePlace is where a peer's line is.
type LinePlace struct {
	Country *string `json:"country"`
	City    *string `json:"city"`
	ISP     *string `json:"isp"`
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

// region is a country or a city and how many free lines it has, as GET
// /v1/lines/regions answers.
type region struct {
	Country string   `json:"country,omitempty"`
	City    string   `json:"city,omitempty"`
	Lines   int      `json:"lines"`
	Cities  []region `json:"cities,omitempty"`
}

// listRegions counts the online lines offered, per country, or with
// ?country= that country's per city, most lines first. Lines in an unknown
// city count for their country only.
func (s *Server) listRegions(w http.ResponseWriter, r *http.Request) {
	m := s.memberRef(r)
	if m == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.verified(m) {
		writeError(w, http.StatusForbidden, "identity_required")
		return
	}
	country := r.URL.Query().Get("country")
	s.mu.Lock()
	defer s.mu.Unlock()
	// The line of the member's own peer peer_id doesn't count.
	except := ""
	for _, p := range s.peers {
		if p.PeerID == r.URL.Query().Get("peer_id") && p.memberID == m.MemberID && !p.deleted {
			except = p.LineID
		}
	}
	countries := map[string]*region{}
	for _, l := range s.offered {
		if l.LineID == except {
			continue
		}
		// The API lists no line whose country it doesn't know.
		if !l.Online || l.Country == "" || (country != "" && l.Country != country) {
			continue
		}
		c := countries[l.Country]
		if c == nil {
			c = &region{Country: l.Country}
			countries[l.Country] = c
		}
		c.Lines++
		if country == "" || l.City == nil || *l.City == "" {
			continue
		}
		i := slices.IndexFunc(c.Cities, func(x region) bool { return x.City == *l.City })
		if i < 0 {
			c.Cities = append(c.Cities, region{City: *l.City})
			i = len(c.Cities) - 1
		}
		c.Cities[i].Lines++
	}
	out := []region{}
	for _, c := range countries {
		if country != "" && c.Cities == nil {
			c.Cities = []region{}
		}
		slices.SortFunc(c.Cities, func(a, b region) int {
			if a.Lines != b.Lines {
				return b.Lines - a.Lines
			}
			return strings.Compare(a.City, b.City)
		})
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b region) int { return strings.Compare(a.Country, b.Country) })
	writeJSON(w, http.StatusOK, map[string]any{"countries": out})
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
		RequestID string  `json:"request_id"`
		LineID    string  `json:"line_id"`
		Country   string  `json:"country"`
		City      *string `json:"city"`
		Name      string  `json:"name"`
		PublicKey string  `json:"public_key"`
	}
	raw, ok := decodeStrict(w, r, &body)
	// The contract's oneOf: one line, or a region (a country, maybe a city).
	oneLine := body.LineID != "" && body.Country == "" && body.City == nil
	inRegion := body.LineID == "" && body.Country != "" && (body.City == nil || *body.City != "")
	if !ok || !uuidV7.MatchString(body.RequestID) || !(oneLine || inRegion) ||
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
	line, found := s.peerLineLocked(body.LineID, body.Country, body.City, "")
	if !found && oneLine {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if !found {
		writeError(w, http.StatusConflict, "no_free_line")
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
		LineID:    line.LineID,
		Line:      LinePlace{Country: &line.Country, City: line.City, ISP: line.ISP},
		Failover:  "country",
		HTTPSOnly: true,
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

// peerLineLocked finds the line a peer goes on: lineID, or else the online
// line in country (and city), other than except, with the fewest peers.
func (s *Server) peerLineLocked(lineID, country string, city *string, except string) (AvailableLine, bool) {
	if lineID != "" {
		i := slices.IndexFunc(s.offered, func(l AvailableLine) bool { return l.LineID == lineID })
		if i < 0 {
			return AvailableLine{}, false
		}
		return s.offered[i], true
	}
	var best AvailableLine
	bestPeers := -1
	for _, l := range s.offered {
		if !l.Online || l.LineID == except || l.Country != country || (city != nil && (l.City == nil || *l.City != *city)) {
			continue
		}
		n := 0
		for _, p := range s.peers {
			if !p.deleted && p.LineID == l.LineID {
				n++
			}
		}
		if bestPeers < 0 || n < bestPeers {
			best, bestPeers = l, n
		}
	}
	return best, bestPeers >= 0
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

// updatePeer moves a peer to another line, or changes its failover or
// HTTPS only. Unlike
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
		LineID    *string `json:"line_id"`
		Country   *string `json:"country"`
		City      *string `json:"city"`
		Failover  *string `json:"failover"`
		HTTPSOnly *bool   `json:"https_only"`
	}
	if _, ok := decodeStrict(w, r, &body); !ok ||
		(body.LineID == nil && body.Country == nil && body.Failover == nil && body.HTTPSOnly == nil) ||
		(body.LineID != nil && (*body.LineID == "" || body.Country != nil)) ||
		(body.Country != nil && *body.Country == "") || (body.City != nil && (body.Country == nil || *body.City == "")) ||
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
	if body.Country != nil {
		// A move to a region: another free line there, never its own.
		line, found := s.peerLineLocked("", *body.Country, body.City, p.LineID)
		if !found {
			writeError(w, http.StatusConflict, "no_free_line")
			return
		}
		p.LineID = line.LineID
		p.Line = LinePlace{Country: &line.Country, City: line.City, ISP: line.ISP}
	}
	if body.LineID != nil {
		p.LineID = *body.LineID
	}
	if body.Failover != nil {
		p.Failover = *body.Failover
	}
	if body.HTTPSOnly != nil {
		p.HTTPSOnly = *body.HTTPSOnly
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
