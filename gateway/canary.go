package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// DefaultCanaryInterval is the mean time between checks of one line when
// CanaryConfig.Interval is zero.
const DefaultCanaryInterval = 15 * time.Minute

// The canary suspends at most CanaryMaxSuspensions lines per
// CanarySuspensionWindow. More lines failing at once points at Homeport (a
// changed zone, a broken page) rather than at their owners, so the rest are
// logged instead of suspended.
const (
	CanaryMaxSuspensions   = 5
	CanarySuspensionWindow = 10 * time.Minute
)

const (
	canaryTimeout     = 20 * time.Second // one check of one line, both kinds
	canaryParallel    = 16               // checks running at once
	canaryMaxBody     = 64 << 10
	canaryContentType = "text/html; charset=utf-8"
	canaryPath        = "/homeport-canary/"
	canaryWarnEvery   = 10 * time.Minute
	// canarySuspectFor is how long a line stays suspect after a mismatch
	// whose confirming check could not run.
	canarySuspectFor = time.Hour
)

// CanaryConfig makes the gateway check every online line now and then for
// a line that changes what passes through it: a modified agent, or an ISP's
// transparent proxy. Each check fetches the gateway's own canary page over
// plain HTTP through the line, and looks up a random name in DNSZone
// through the line's DNS. A response that differs from the known one
// suspends the line (see Server.LineEvent's Tampered). A line that only
// fails to answer is left alone: it is broken, not proven to tamper.
type CanaryConfig struct {
	// HTTPListen is where the gateway serves the canary page over plain
	// HTTP, such as ":80". HTTPTarget is the public address that reaches
	// it, which checks fetch through the line. The zero HTTPTarget means
	// the listener's own address, which suits tests only.
	HTTPListen string
	HTTPTarget netip.AddrPort

	// DNSZone is a Homeport zone with a wildcard record, so that every name
	// under it resolves to the same public addresses. Empty skips the DNS
	// check. Resolver is the gateway's own resolver for it; nil means
	// net.DefaultResolver.
	DNSZone  string
	Resolver Resolver

	// Interval is the mean time between checks of one line. Zero means
	// DefaultCanaryInterval. A line's first check comes within the first
	// interval, or two minutes, after its agent connects.
	Interval time.Duration

	// Logger gets the canary's warnings. Nil discards them.
	Logger *slog.Logger
}

// Resolver looks up a host's addresses. *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// canary checks the server's lines. Every check runs a control first,
// directly from the gateway: if the gateway itself does not get the known
// page or a public answer for the zone, the check is skipped, so a
// misconfiguration never suspends a line.
type canary struct {
	s        *Server
	cfg      CanaryConfig
	ln       net.Listener
	page     *http.Server
	target   netip.AddrPort
	zone     string
	interval time.Duration
	log      *slog.Logger
	cancel   context.CancelFunc
	done     sync.WaitGroup
	lastWarn atomic.Int64

	mu        sync.Mutex
	suspended []time.Time // recent suspensions, for the breaker
	// suspects are line ID + check kind with an unconfirmed mismatch, and
	// when it happened.
	suspects map[string]time.Time
}

// canaryResult is the outcome of one check.
type canaryResult int

const (
	canaryInconclusive canaryResult = iota // no answer, or the control failed
	canaryPassed
	canaryTampered
)

func startCanary(s *Server, cfg CanaryConfig) (*canary, error) {
	if s.relay == nil {
		return nil, errors.New("canary: needs the relay to agents (Config.QUICListen)")
	}
	ln, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return nil, fmt.Errorf("canary: listen: %w", err)
	}
	c := &canary{s: s, cfg: cfg, ln: ln, target: cfg.HTTPTarget, interval: cfg.Interval, log: cfg.Logger, suspects: map[string]time.Time{}}
	if !c.target.IsValid() {
		c.target = netip.MustParseAddrPort(ln.Addr().String())
	}
	if c.interval <= 0 {
		c.interval = DefaultCanaryInterval
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.cfg.Resolver == nil {
		c.cfg.Resolver = net.DefaultResolver
	}
	if cfg.DNSZone != "" {
		c.zone = strings.TrimSuffix(strings.ToLower(cfg.DNSZone), ".") + "."
	}
	c.page = &http.Server{Handler: http.HandlerFunc(serveCanaryPage), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = c.page.Serve(ln) }()
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done.Go(func() { c.run(ctx) })
	return c, nil
}

func (c *canary) close() {
	c.cancel()
	_ = c.page.Close()
	c.done.Wait()
}

// run starts each online line's checks when they are due.
func (c *canary) run(ctx context.Context) {
	tick := time.NewTicker(min(max(c.interval/5, 10*time.Millisecond), 5*time.Second))
	defer tick.Stop()
	due := map[string]time.Time{}
	var running sync.Map // line ID → struct{}
	slots := make(chan struct{}, canaryParallel)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			online := c.s.relay.OnlineLines()
			isOnline := make(map[string]bool, len(online))
			for _, id := range online {
				isOnline[id] = true
			}
			for id := range due {
				if !isOnline[id] {
					delete(due, id)
				}
			}
			for _, id := range online {
				at, ok := due[id]
				if !ok {
					due[id] = now.Add(jitter(min(c.interval, 2*time.Minute)))
					continue
				}
				if now.Before(at) {
					continue
				}
				if _, busy := running.LoadOrStore(id, struct{}{}); busy {
					continue
				}
				due[id] = now.Add(c.interval/2 + jitter(c.interval)) // a mean of interval
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				c.done.Go(func() {
					defer func() { <-slots; running.Delete(id) }()
					c.check(ctx, id)
				})
			}
		}
	}
}

// check runs one line's checks and suspends the line if one shows it
// changed a response. The agent's address and release are taken first: the
// report needs them, and an agent that disconnects right after it tampered
// must not escape it.
func (c *canary) check(ctx context.Context, lineID string) {
	ctx, cancel := context.WithTimeout(ctx, canaryTimeout)
	defer cancel()
	ip, version, ok := c.s.relay.Agent(lineID)
	if !ok {
		return
	}
	switch {
	case c.confirmed(ctx, lineID, "http", c.checkHTTP):
		c.suspend(lineID, "http", ip, version)
	case c.zone != "" && c.confirmed(ctx, lineID, "dns", c.checkDNS):
		c.suspend(lineID, "dns", ip, version)
	}
}

// confirmed reports whether check shows the line tampering. A mismatch
// counts only when a second check, with a new nonce and a new control,
// mismatches too, so one odd answer is forgiven. When the second check
// cannot run (the agent left right after the mismatch, say), the line
// becomes suspect for canarySuspectFor: its next mismatch counts at once.
// A check that passes clears that.
func (c *canary) confirmed(ctx context.Context, lineID, kind string, check func(context.Context, string) canaryResult) bool {
	key := lineID + " " + kind
	first := check(ctx, lineID)
	c.mu.Lock()
	suspect := time.Since(c.suspects[key]) < canarySuspectFor
	c.mu.Unlock()
	if first != canaryTampered {
		if first == canaryPassed {
			c.setSuspect(key, false)
		}
		return false
	}
	if suspect {
		return true
	}
	switch check(ctx, lineID) {
	case canaryTampered:
		return true
	case canaryPassed:
		c.setSuspect(key, false)
	default:
		c.setSuspect(key, true)
	}
	return false
}

func (c *canary) setSuspect(key string, suspect bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if suspect {
		c.suspects[key] = time.Now()
	} else {
		delete(c.suspects, key)
	}
}

// suspend suspends a line that a check caught, unless the breaker holds:
// CanaryMaxSuspensions lines suspended within CanarySuspensionWindow.
func (c *canary) suspend(lineID, check string, ip netip.Addr, version string) {
	now := time.Now()
	c.mu.Lock()
	c.suspended = slices.DeleteFunc(c.suspended, func(at time.Time) bool { return now.Sub(at) >= CanarySuspensionWindow })
	full := len(c.suspended) >= CanaryMaxSuspensions
	if !full {
		c.suspended = append(c.suspended, now)
	}
	c.mu.Unlock()
	if full {
		c.log.Error("canary check failed, but too many lines failed at once to blame their owners; check the canary page and zone",
			"line_id", lineID, "check", check)
		return
	}
	c.s.suspendLine(lineID, check, ip, version)
	c.log.Warn("line suspended: a canary check got a changed answer through it, twice", "line_id", lineID, "check", check)
}

// checkHTTP fetches the canary page with a fresh nonce, directly and then
// through the line, and compares the line's response with the page.
func (c *canary) checkHTTP(ctx context.Context, lineID string) canaryResult {
	nonce := newNonce()
	var d net.Dialer
	direct, err := d.DialContext(ctx, "tcp", c.target.String())
	if err != nil {
		c.warn("canary page unreachable from the gateway; skipping HTTP checks", err)
		return canaryInconclusive
	}
	res, err := c.fetch(ctx, direct, nonce)
	_ = direct.Close()
	if err != nil || !res.isPage(nonce) {
		c.warn("canary page address serves something else; skipping HTTP checks", err)
		return canaryInconclusive
	}

	conn, err := c.s.relay.DialTCP(ctx, lineID, c.target)
	if err != nil {
		return canaryInconclusive
	}
	defer func() { _ = conn.Close() }()
	res, err = c.fetch(ctx, conn, nonce)
	switch {
	case err != nil:
		return canaryInconclusive // the line broke the connection or never answered
	case res.isPage(nonce):
		return canaryPassed
	default:
		return canaryTampered
	}
}

type canaryResponse struct {
	status      int
	contentType string
	body        []byte
}

func (r canaryResponse) isPage(nonce string) bool {
	return r.status == http.StatusOK && r.contentType == canaryContentType && bytes.Equal(r.body, canaryPage(nonce))
}

// fetch sends the canary request on conn, as a browser would, and reads
// the response.
func (c *canary) fetch(ctx context.Context, conn net.Conn, nonce string) (canaryResponse, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	req := fmt.Sprintf("GET %s%s HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0\r\nAccept: text/html\r\nAccept-Encoding: identity\r\nConnection: close\r\n\r\n",
		canaryPath, nonce, c.target.Addr())
	if _, err := io.WriteString(conn, req); err != nil {
		return canaryResponse{}, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return canaryResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, canaryMaxBody))
	if err != nil {
		return canaryResponse{}, err
	}
	return canaryResponse{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: body}, nil
}

// checkDNS looks up a random name in the zone, directly and then through
// the line, and compares the line's answer with the gateway's own.
func (c *canary) checkDNS(ctx context.Context, lineID string) canaryResult {
	name := newNonce() + "." + c.zone
	want, err := c.cfg.Resolver.LookupNetIP(ctx, "ip4", name)
	for i := range want {
		want[i] = want[i].Unmap()
	}
	if err != nil || len(want) == 0 || slices.ContainsFunc(want, relayprotocol.SpecialPurpose) {
		c.warn("canary zone does not resolve to public addresses from the gateway; skipping DNS checks", err)
		return canaryInconclusive
	}
	conn, err := c.s.relay.DialUDP(ctx, lineID, netip.AddrPortFrom(c.s.cfg.DNS, 53))
	if err != nil {
		return canaryInconclusive
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var idb [2]byte
	_, _ = rand.Read(idb[:])
	id := binary.BigEndian.Uint16(idb[:])
	q, err := canaryQuery(id, name)
	if err != nil {
		return canaryInconclusive
	}
	if _, err := conn.Write(q); err != nil {
		return canaryInconclusive
	}
	buf := make([]byte, relayprotocol.MaxUDPPayload)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return canaryInconclusive
		}
		rcode, got, ok := parseCanaryAnswer(buf[:n], id)
		if !ok {
			continue // not the answer to this query
		}
		switch rcode {
		case dnsmessage.RCodeSuccess, dnsmessage.RCodeNameError:
			// An honest agent may leave addresses out (it keeps answers
			// within 512 bytes and drops those its policy refuses), but
			// never answers with none, or with one the zone does not have.
			if rcode == dnsmessage.RCodeSuccess && len(got) > 0 && subset(got, want) {
				return canaryPassed
			}
			return canaryTampered
		default:
			return canaryInconclusive // SERVFAIL and the like: the line's DNS is broken
		}
	}
}

func canaryQuery(id uint16, name string) ([]byte, error) {
	n, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(dnsmessage.Question{Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		return nil, err
	}
	return b.Finish()
}

// parseCanaryAnswer returns the RCode and A records of a response to the
// query with id. ok is false for anything else.
func parseCanaryAnswer(msg []byte, id uint16) (rcode dnsmessage.RCode, addrs []netip.Addr, ok bool) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || !h.Response || h.ID != id {
		return 0, nil, false
	}
	if err := p.SkipAllQuestions(); err != nil {
		return 0, nil, false
	}
	for {
		rh, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			return h.RCode, addrs, true
		}
		if err != nil {
			return 0, nil, false
		}
		if rh.Type != dnsmessage.TypeA {
			if p.SkipAnswer() != nil {
				return 0, nil, false
			}
			continue
		}
		a, err := p.AResource()
		if err != nil {
			return 0, nil, false
		}
		addrs = append(addrs, netip.AddrFrom4(a.A))
	}
}

// subset reports whether every address in a is in b.
func subset(a, b []netip.Addr) bool {
	for _, addr := range a {
		if !slices.Contains(b, addr) {
			return false
		}
	}
	return true
}

// warn logs a control failure at most once per canaryWarnEvery.
func (c *canary) warn(msg string, err error) {
	now := time.Now().UnixNano()
	last := c.lastWarn.Load()
	if now-last < int64(canaryWarnEvery) || !c.lastWarn.CompareAndSwap(last, now) {
		return
	}
	c.log.Warn(msg, "error", err)
}

var canaryNonce = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

// serveCanaryPage answers GET /homeport-canary/<nonce> with the page for
// that nonce.
func serveCanaryPage(w http.ResponseWriter, r *http.Request) {
	nonce, ok := strings.CutPrefix(r.URL.Path, canaryPath)
	if r.Method != http.MethodGet || !ok || !canaryNonce.MatchString(nonce) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", canaryContentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(canaryPage(nonce)) // #nosec G705 -- the nonce is checked to be hex
}

// canaryPage is an ordinary small HTML page, so that a proxy which injects
// into HTML treats it like any other.
func canaryPage(nonce string) []byte {
	return []byte("<!doctype html>\n<html><head><meta charset=\"utf-8\"><title>Homeport</title></head>\n<body><p>" + nonce + "</p></body></html>\n")
}

// newNonce returns 32 random hex digits: a fresh path and DNS label for
// each check, so no cache answers it and nothing can be replayed.
func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// jitter returns a random duration in [0, d).
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return mrand.N(d) // #nosec G404 -- spreads checks out; not a secret
}
