package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/x509roots/fallback/bundle"
	"golang.org/x/net/dns/dnsmessage"
)

// dohServer is a DNS-over-HTTPS resolver (RFC 8484). The agent connects to
// Addrs directly, so finding the resolver needs no DNS, and checks that the
// certificate names the URL's host.
type dohServer struct {
	URL   string
	Addrs []netip.AddrPort
}

// defaultDoHServers are the resolvers the agent trusts, in order. Neither
// sends EDNS Client Subnet, and both answer from the point of presence
// nearest the line, so websites still get answers for the line's region.
var defaultDoHServers = []dohServer{
	{
		URL: "https://cloudflare-dns.com/dns-query",
		Addrs: []netip.AddrPort{
			netip.MustParseAddrPort("1.1.1.1:443"),
			netip.MustParseAddrPort("1.0.0.1:443"),
			netip.MustParseAddrPort("[2606:4700:4700::1111]:443"),
			netip.MustParseAddrPort("[2606:4700:4700::1001]:443"),
		},
	},
	{
		URL: "https://dns.quad9.net/dns-query",
		Addrs: []netip.AddrPort{
			netip.MustParseAddrPort("9.9.9.9:443"),
			netip.MustParseAddrPort("149.112.112.112:443"),
			netip.MustParseAddrPort("[2620:fe::fe]:443"),
			netip.MustParseAddrPort("[2620:fe::9]:443"),
		},
	},
}

const (
	// dohAttemptTimeout bounds one query to one resolver, including a new
	// connection (TCP, TLS and the request: three round trips, which a
	// satellite line takes over a second for), and leaves time for the
	// next resolver within dnsQueryTimeout.
	dohAttemptTimeout = 2500 * time.Millisecond
	// dohDialStagger is how long a connection attempt to one address runs
	// alone before the next address is tried alongside it, so a lost SYN
	// costs a fraction of a second rather than a retransmission timeout.
	dohDialStagger = 250 * time.Millisecond
	// dohDialTimeout bounds one connection to a resolver. net/http keeps
	// dialing after the query that started it gave up, so without it an
	// address that swallows SYNs would hold a socket until the OS's TCP
	// timeout, a minute or more.
	dohDialTimeout = dohAttemptTimeout
	// dohWarnInterval spaces out the warning that no resolver answered.
	dohWarnInterval = time.Minute
	// dohMaxResponse is the largest DNS message there is.
	dohMaxResponse = 65535
	// dohPadBlock is the block size queries are padded to (RFC 8467).
	dohPadBlock = 128
	// dnsMessage is the media type of DoH requests and responses.
	dnsMessage = "application/dns-message"
)

// bundledRoots are the Mozilla root certificates built into the agent. The
// agent verifies resolvers against these instead of the machine's trust
// store, so a root the member, their employer or a proxy has installed on
// the machine cannot intercept DNS.
var bundledRoots = sync.OnceValue(func() *x509.CertPool {
	pool := x509.NewCertPool()
	for root := range bundle.Roots() {
		cert, err := x509.ParseCertificate(root.Certificate)
		if err != nil {
			continue
		}
		if root.Constraint == nil {
			pool.AddCert(cert)
		} else {
			pool.AddCertWithConstraint(cert, root.Constraint)
		}
	}
	return pool
})

// dohResolver resolves names over DNS over HTTPS. Its queries leave
// through the line as ordinary TLS connections, so the resolver sees the
// line's address, but neither the machine's network nor its ISP can read
// or change the answers. It never falls back to the system resolver: that
// would let anyone who blocks the resolvers forge answers again.
type dohResolver struct {
	servers        []dohServer
	roots          *x509.CertPool
	clients        []*http.Client
	attemptTimeout time.Duration
	dialTimeout    time.Duration
	preferred      atomic.Int64 // the server that answered last
	// dialAddr opens a TCP connection to one address. Tests replace it.
	dialAddr func(ctx context.Context, addr netip.AddrPort) (net.Conn, error)
	// log gets a warning, naming no DNS name, when no resolver answers.
	log      *slog.Logger
	lastWarn atomic.Int64 // when it last warned, in Unix nanoseconds
}

func newDoHResolver(servers []dohServer, roots *x509.CertPool) *dohResolver {
	var d net.Dialer
	r := &dohResolver{
		servers:        servers,
		roots:          roots,
		attemptTimeout: dohAttemptTimeout,
		dialTimeout:    dohDialTimeout,
		log:            slog.New(slog.DiscardHandler),
		dialAddr: func(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", addr.String())
		},
	}
	for _, s := range servers {
		r.clients = append(r.clients, &http.Client{Transport: r.transport(s)})
	}
	return r
}

// transport is an HTTP/2 transport that connects to s's addresses whatever
// the URL's host, and verifies the server against r.roots only.
func (r *dohResolver) transport(s dohServer) *http.Transport {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return r.dialFirst(ctx, s.Addrs)
	}
	return &http.Transport{
		DialContext:       dial,
		TLSClientConfig:   &tls.Config{RootCAs: r.roots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
		// One HTTP/2 connection carries every query. Queries that find none
		// wait for the one being dialed instead of each dialing their own.
		MaxConnsPerHost: 1,
		IdleConnTimeout: time.Minute,
		// A connection that went dead, as when the machine changed networks,
		// is noticed and replaced instead of holding up lookups.
		HTTP2: &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 5 * time.Second},
	}
}

// CloseIdleConnections closes the resolver's connections and ends any
// connection attempt still running. The agent calls it when Run returns.
func (r *dohResolver) CloseIdleConnections() {
	for _, c := range r.clients {
		c.CloseIdleConnections()
	}
}

// dialFirst returns a connection to the first of addrs that accepts one. It
// dials them in order, starting the next whenever one fails or has run for
// dohDialStagger without connecting, and closes the connections that lose.
func (r *dohResolver) dialFirst(ctx context.Context, addrs []netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, r.dialTimeout)
	defer cancel() // ends the attempts that are still running
	type result struct {
		conn net.Conn
		err  error
	}
	results := make(chan result)
	stagger := time.NewTimer(0)
	defer stagger.Stop()
	var errs []error
	next, running := 0, 0
	for next < len(addrs) || running > 0 {
		var start <-chan time.Time
		if next < len(addrs) {
			start = stagger.C
		}
		select {
		case <-start:
			addr := addrs[next]
			next++
			running++
			go func() {
				c, err := r.dialAddr(ctx, addr)
				select {
				case results <- result{c, err}:
				case <-ctx.Done():
					if c != nil {
						_ = c.Close()
					}
				}
			}()
			stagger.Reset(dohDialStagger)
		case res := <-results:
			running--
			if res.err == nil {
				return res.conn, nil
			}
			errs = append(errs, res.err)
			stagger.Reset(0) // a failed address hands over at once
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if len(errs) == 0 {
		return nil, errors.New("doh: no address to dial")
	}
	return nil, errors.Join(errs...)
}

// LookupNetIP implements Resolver for "ip4" (A) and "ip6" (AAAA). Like
// net.Resolver, it reports a missing name or no records of that family as
// a *net.DNSError with IsNotFound.
func (r *dohResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	var typ dnsmessage.Type
	switch network {
	case "ip4":
		typ = dnsmessage.TypeA
	case "ip6":
		typ = dnsmessage.TypeAAAA
	default:
		return nil, fmt.Errorf("doh: unsupported network %q", network)
	}
	name, err := dnsmessage.NewName(host)
	if err != nil || !strings.HasSuffix(host, ".") {
		return nil, &net.DNSError{Err: "invalid name"}
	}
	q := dnsmessage.Question{Name: name, Type: typ, Class: dnsmessage.ClassINET}
	msg, err := dohQuery(q)
	if err != nil {
		return nil, err
	}
	var errs []error
	first := int(r.preferred.Load())
	for i := range r.servers {
		n := (first + i) % len(r.servers)
		addrs, err := r.ask(ctx, n, msg, q)
		if err == nil || isNotFoundErr(err) {
			r.preferred.Store(int64(n))
			return addrs, err
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	err = fmt.Errorf("no resolver answered: %w", errors.Join(errs...))
	r.warn(err)
	return nil, &net.DNSError{Err: err.Error(), IsTemporary: true}
}

// warn logs that no resolver answered, at most once per dohWarnInterval:
// a network that blocks DNS over HTTPS fails every lookup, and the member
// should learn why the line resolves nothing. err names the resolvers and
// the reasons, never the name that was looked up.
func (r *dohResolver) warn(err error) {
	now := time.Now().UnixNano()
	last := r.lastWarn.Load()
	if now-last < int64(dohWarnInterval) || !r.lastWarn.CompareAndSwap(last, now) {
		return
	}
	r.log.Warn("dns over https: no resolver answered; the network may block it", "error", err)
}

// ask sends msg to server n and returns the addresses in its answer.
func (r *dohResolver) ask(ctx context.Context, n int, msg []byte, q dnsmessage.Question) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, r.attemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.servers[n].URL, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", dnsMessage)
	req.Header.Set("Accept", dnsMessage)
	resp, err := r.clients[n].Do(req)
	if err != nil {
		return nil, errors.New(redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); resp.StatusCode != http.StatusOK || mt != dnsMessage {
		return nil, fmt.Errorf("doh: status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, dohMaxResponse+1))
	if err != nil {
		return nil, fmt.Errorf("doh: read response: %w", err)
	}
	if len(body) > dohMaxResponse {
		return nil, errors.New("doh: response too large")
	}
	return parseDoHAnswer(body, q)
}

// dohQuery builds the query for q: ID 0 (RFC 8484, so answers can be
// cached), recursion desired, a UDP size of 65535, which over HTTPS means
// "no limit" (RFC 8484 section 6), and EDNS(0) padding to a multiple of
// dohPadBlock bytes (RFC 8467), so the size of the encrypted query does not
// give the name away.
func dohQuery(q dnsmessage.Question) ([]byte, error) {
	build := func(pad int) ([]byte, error) {
		b := dnsmessage.NewBuilder(make([]byte, 0, dohPadBlock*2), dnsmessage.Header{RecursionDesired: true})
		if err := b.StartQuestions(); err != nil {
			return nil, err
		}
		if err := b.Question(q); err != nil {
			return nil, err
		}
		if err := b.StartAdditionals(); err != nil {
			return nil, err
		}
		var rh dnsmessage.ResourceHeader
		if err := rh.SetEDNS0(dohMaxResponse, dnsmessage.RCodeSuccess, false); err != nil {
			return nil, err
		}
		opt := dnsmessage.OPTResource{Options: []dnsmessage.Option{{Code: 12, Data: make([]byte, pad)}}}
		if err := b.OPTResource(rh, opt); err != nil {
			return nil, err
		}
		return b.Finish()
	}
	msg, err := build(0)
	if err != nil {
		return nil, err
	}
	return build((dohPadBlock - len(msg)%dohPadBlock) % dohPadBlock)
}

// parseDoHAnswer returns the addresses of q's type in an answer to q. A
// name error, or an answer without such records, is not found.
func parseDoHAnswer(msg []byte, q dnsmessage.Question) ([]netip.Addr, error) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return nil, fmt.Errorf("doh: parse answer: %w", err)
	}
	if !h.Response || h.ID != 0 {
		return nil, errors.New("doh: not an answer to the query")
	}
	if h.Truncated {
		return nil, errors.New("doh: truncated answer")
	}
	questions, err := p.AllQuestions()
	if err != nil || len(questions) != 1 || questions[0].Type != q.Type || questions[0].Class != q.Class ||
		!strings.EqualFold(questions[0].Name.String(), q.Name.String()) {
		return nil, errors.New("doh: the answer is for another question")
	}
	switch h.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError:
		return nil, notFound(q)
	default:
		return nil, fmt.Errorf("doh: %v", h.RCode)
	}
	var addrs []netip.Addr
	for {
		rh, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("doh: parse answer: %w", err)
		}
		switch {
		case rh.Type == dnsmessage.TypeA && q.Type == dnsmessage.TypeA:
			a, err := p.AResource()
			if err != nil {
				return nil, fmt.Errorf("doh: parse answer: %w", err)
			}
			addrs = append(addrs, netip.AddrFrom4(a.A))
		case rh.Type == dnsmessage.TypeAAAA && q.Type == dnsmessage.TypeAAAA:
			a, err := p.AAAAResource()
			if err != nil {
				return nil, fmt.Errorf("doh: parse answer: %w", err)
			}
			addrs = append(addrs, netip.AddrFrom16(a.AAAA))
		default:
			// CNAMEs lead to the records above; anything else is not asked for.
			if err := p.SkipAnswer(); err != nil {
				return nil, fmt.Errorf("doh: parse answer: %w", err)
			}
		}
	}
	if len(addrs) == 0 {
		return nil, notFound(q)
	}
	return addrs, nil
}

// notFound reports that q has no answer. Like every error from here, it
// names no DNS name, so it is safe to log.
func notFound(dnsmessage.Question) error {
	return &net.DNSError{Err: "no such host", IsNotFound: true}
}

func isNotFoundErr(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// redactURLError keeps the reason of an HTTP client error but drops the
// request URL it wraps, so the error stays short: the reason is what a log
// needs.
func redactURLError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return "doh: " + uerr.Err.Error()
	}
	return "doh: " + err.Error()
}
