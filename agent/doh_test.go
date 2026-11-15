package agent

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// dohAnswer is what the fake DoH server answers for one name and type.
type dohAnswer struct {
	rcode dnsmessage.RCode
	cname string       // a CNAME to answer first, if set
	addrs []netip.Addr // A or AAAA records, owned by cname if set
	name  string       // the question name to answer with, if not the asked one
}

// fakeDoH is a DNS-over-HTTPS server (RFC 8484) answering from a table
// keyed by "name type", such as "example.org. A".
type fakeDoH struct {
	srv     *httptest.Server
	answers map[string]dohAnswer
	status  atomic.Int32 // an HTTP status to fail every request with, if set
	stall   atomic.Bool  // hold every request until it is cancelled

	mu      sync.Mutex
	queries [][]byte
	headers []http.Header
}

func newFakeDoH(t *testing.T, answers map[string]dohAnswer) *fakeDoH {
	t.Helper()
	f := &fakeDoH{answers: answers}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.EnableHTTP2 = true
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

// server is f as a dohServer the resolver can use: example.com, which the
// test certificate names, at the test server's address.
func (f *fakeDoH) server() dohServer {
	return dohServer{
		URL:   "https://example.com/dns-query",
		Addrs: []netip.AddrPort{netip.MustParseAddrPort(f.srv.Listener.Addr().String())},
	}
}

// roots trusts f's certificate only.
func (f *fakeDoH) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	return pool
}

func (f *fakeDoH) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.queries)
}

func (f *fakeDoH) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.queries = append(f.queries, body)
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	if f.stall.Load() {
		<-r.Context().Done()
		return
	}
	if code := f.status.Load(); code != 0 {
		http.Error(w, "unavailable", int(code))
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/dns-query" || r.Header.Get("Content-Type") != "application/dns-message" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var p dnsmessage.Parser
	h, err := p.Start(body)
	if err != nil {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	q, err := p.Question()
	if err != nil {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	ans := f.answers[q.Name.String()+" "+strings.TrimPrefix(q.Type.String(), "Type")]
	if ans.name != "" {
		q.Name = dnsmessage.MustNewName(ans.name)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionDesired: true, RecursionAvailable: true, RCode: ans.rcode})
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	owner := q.Name
	if ans.cname != "" {
		target := dnsmessage.MustNewName(ans.cname)
		_ = b.CNAMEResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 300}, dnsmessage.CNAMEResource{CNAME: target})
		owner = target
	}
	for _, a := range ans.addrs {
		rh := dnsmessage.ResourceHeader{Name: owner, Class: dnsmessage.ClassINET, TTL: 300}
		if a.Is4() {
			_ = b.AResource(rh, dnsmessage.AResource{A: a.As4()})
		} else {
			_ = b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: a.As16()})
		}
	}
	msg, err := b.Finish()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/dns-message")
	_, _ = w.Write(msg)
}

func resolveWith(t *testing.T, r Resolver, network, host string) ([]netip.Addr, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	return r.LookupNetIP(ctx, network, host)
}

func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

var (
	public4  = netip.MustParseAddr("11.22.33.44")
	public4b = netip.MustParseAddr("11.22.33.45")
	public6  = netip.MustParseAddr("2a00:1450:4001::1")
)

func TestDoHLooksUpAAndAAAARecords(t *testing.T) {
	f := newFakeDoH(t, map[string]dohAnswer{
		"example.org. A":    {addrs: []netip.Addr{public4, public4b}},
		"example.org. AAAA": {addrs: []netip.Addr{public6}},
	})
	r := newDoHResolver([]dohServer{f.server()}, f.roots())

	if got, err := resolveWith(t, r, "ip4", "example.org."); err != nil || !slices.Equal(got, []netip.Addr{public4, public4b}) {
		t.Errorf("ip4 lookup = %v, %v", got, err)
	}
	if got, err := resolveWith(t, r, "ip6", "example.org."); err != nil || !slices.Equal(got, []netip.Addr{public6}) {
		t.Errorf("ip6 lookup = %v, %v", got, err)
	}
}

func TestDoHFollowsACNAMEAnswer(t *testing.T) {
	f := newFakeDoH(t, map[string]dohAnswer{
		"www.example.org. A": {cname: "edge.cdn.example.", addrs: []netip.Addr{public4}},
	})
	r := newDoHResolver([]dohServer{f.server()}, f.roots())

	if got, err := resolveWith(t, r, "ip4", "www.example.org."); err != nil || !slices.Equal(got, []netip.Addr{public4}) {
		t.Errorf("lookup = %v, %v", got, err)
	}
}

func TestDoHNameErrorsAndEmptyAnswersAreNotFound(t *testing.T) {
	f := newFakeDoH(t, map[string]dohAnswer{
		"gone.example.org. A":      {rcode: dnsmessage.RCodeNameError},
		"v4only.example.org. AAAA": {},
	})
	r := newDoHResolver([]dohServer{f.server()}, f.roots())

	for _, q := range [][2]string{{"ip4", "gone.example.org."}, {"ip6", "v4only.example.org."}} {
		if got, err := resolveWith(t, r, q[0], q[1]); !isNotFound(err) {
			t.Errorf("%s %s = %v, %v; want not found", q[0], q[1], got, err)
		}
	}
}

func TestDoHServerFailureIsAnError(t *testing.T) {
	f := newFakeDoH(t, map[string]dohAnswer{"broken.example.org. A": {rcode: dnsmessage.RCodeServerFailure}})
	r := newDoHResolver([]dohServer{f.server()}, f.roots())

	if got, err := resolveWith(t, r, "ip4", "broken.example.org."); err == nil || isNotFound(err) {
		t.Errorf("lookup = %v, %v; want an error that is not 'not found'", got, err)
	}
}

func TestDoHTriesTheNextServerAndThenPrefersIt(t *testing.T) {
	answers := map[string]dohAnswer{"example.org. A": {addrs: []netip.Addr{public4}}}
	down := newFakeDoH(t, answers)
	down.status.Store(http.StatusServiceUnavailable)
	up := newFakeDoH(t, answers)
	roots := down.roots()
	roots.AddCert(up.srv.Certificate())
	r := newDoHResolver([]dohServer{down.server(), up.server()}, roots)

	for range 3 {
		if got, err := resolveWith(t, r, "ip4", "example.org."); err != nil || !slices.Equal(got, []netip.Addr{public4}) {
			t.Fatalf("lookup = %v, %v", got, err)
		}
	}
	if n := len(down.received()); n != 1 {
		t.Errorf("the failed server got %d queries, want 1: later lookups should go to the one that answered", n)
	}
}

func TestDoHMovesOnFromAServerThatDoesNotAnswer(t *testing.T) {
	answers := map[string]dohAnswer{"example.org. A": {addrs: []netip.Addr{public4}}}
	stalled := newFakeDoH(t, answers)
	stalled.stall.Store(true)
	up := newFakeDoH(t, answers)
	roots := stalled.roots()
	roots.AddCert(up.srv.Certificate())
	r := newDoHResolver([]dohServer{stalled.server(), up.server()}, roots)
	r.attemptTimeout = 200 * time.Millisecond

	start := time.Now()
	if got, err := resolveWith(t, r, "ip4", "example.org."); err != nil || !slices.Equal(got, []netip.Addr{public4}) {
		t.Fatalf("lookup = %v, %v", got, err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the lookup took %v; the stalled server should be given up after its attempt timeout", took)
	}
}

func TestDoHDoesNotWaitOutAnAddressThatSwallowsItsConnection(t *testing.T) {
	// The first address drops the SYN, as a lossy line or a filtered
	// address does. The next address is tried long before a connect
	// timeout would give up on the first.
	f := newFakeDoH(t, map[string]dohAnswer{"example.org. A": {addrs: []netip.Addr{public4}}})
	blackhole := netip.MustParseAddrPort("11.22.33.44:443")
	srv := f.server()
	srv.Addrs = append([]netip.AddrPort{blackhole}, srv.Addrs...)
	r := newDoHResolver([]dohServer{srv}, f.roots())
	var d net.Dialer
	r.dialAddr = func(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
		if addr == blackhole {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return d.DialContext(ctx, "tcp", addr.String())
	}

	start := time.Now()
	if got, err := resolveWith(t, r, "ip4", "example.org."); err != nil || !slices.Equal(got, []netip.Addr{public4}) {
		t.Fatalf("lookup = %v, %v", got, err)
	}
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Errorf("the lookup took %v; the second address should be dialed after a short stagger", took)
	}
}

func TestDoHRefusesAServerOutsideItsTrustedRoots(t *testing.T) {
	// The machine's own trust store does not count: a root the seller
	// installs, or an intercepting proxy's, gets no answers through.
	f := newFakeDoH(t, map[string]dohAnswer{"example.org. A": {addrs: []netip.Addr{public4}}})
	r := newDoHResolver([]dohServer{f.server()}, x509.NewCertPool())

	if got, err := resolveWith(t, r, "ip4", "example.org."); err == nil {
		t.Fatalf("lookup through an untrusted server = %v", got)
	}
	if n := len(f.received()); n != 0 {
		t.Errorf("the untrusted server received %d queries", n)
	}
}

func TestDoHRefusesAnAnswerToAnotherQuestion(t *testing.T) {
	f := newFakeDoH(t, map[string]dohAnswer{"example.org. A": {name: "other.example.", addrs: []netip.Addr{public4}}})
	r := newDoHResolver([]dohServer{f.server()}, f.roots())

	if got, err := resolveWith(t, r, "ip4", "example.org."); err == nil || isNotFound(err) {
		t.Errorf("lookup = %v, %v; want an error", got, err)
	}
}

func TestDoHQueriesArePaddedAndHaveIDZero(t *testing.T) {
	f := newFakeDoH(t, map[string]dohAnswer{})
	r := newDoHResolver([]dohServer{f.server()}, f.roots())
	for _, name := range []string{"a.example.", "a-much-longer-name.with.several.labels.example."} {
		_, _ = resolveWith(t, r, "ip4", name)
	}

	queries := f.received()
	if len(queries) != 2 {
		t.Fatalf("got %d queries, want 2", len(queries))
	}
	for _, q := range queries {
		// RFC 8467: pad queries to a multiple of 128 bytes, so their size
		// does not give the name away. RFC 8484: use ID 0.
		if len(q)%128 != 0 {
			t.Errorf("a query is %d bytes, not a multiple of 128", len(q))
		}
		var p dnsmessage.Parser
		h, err := p.Start(q)
		if err != nil || h.ID != 0 || !h.RecursionDesired {
			t.Errorf("query header %+v, %v; want ID 0 with recursion desired", h, err)
		}
	}
	if got := f.headers[0].Get("Accept"); got != "application/dns-message" {
		t.Errorf("Accept = %q", got)
	}
}

func TestTheAgentResolvesOverDoHWithItsOwnTrustStoreByDefault(t *testing.T) {
	cert, err := relayprotocol.GenerateSelfSigned("agent")
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{GatewayAddr: "gateway.test:443", GatewayPin: relayprotocol.PinOf(cert.Leaf), Certificate: cert})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := a.cfg.Resolver.(*dohResolver)
	if !ok {
		t.Fatalf("the default resolver is %T, want DNS over HTTPS", a.cfg.Resolver)
	}
	var urls []string
	for _, s := range r.servers {
		urls = append(urls, s.URL)
	}
	if want := []string{"https://cloudflare-dns.com/dns-query", "https://dns.quad9.net/dns-query"}; !slices.Equal(urls, want) {
		t.Errorf("resolvers = %v, want %v", urls, want)
	}
	sys, _ := x509.SystemCertPool()
	if r.roots == nil || r.roots.Equal(sys) || !r.roots.Equal(bundledRoots()) {
		t.Error("the default resolver should trust the roots built into the agent, not the machine's")
	}
}

func TestDNSFlowsResolveOverDoHAndStillDropHomeAddresses(t *testing.T) {
	f := newFakeDoH(t, map[string]dohAnswer{
		"mixed.homeport.test. A": {addrs: []netip.Addr{netip.MustParseAddr("192.168.178.20"), public4}},
	})
	g := newFakeGateway(t)
	startAgent(t, g, func(c *Config) { c.Resolver = newDoHResolver([]dohServer{f.server()}, f.roots()) })
	s := g.nextConn(t).mustDial(t, relayprotocol.KindDNS, virtualResolver)

	got := exchange(t, s, query(t, 7, "mixed.homeport.test.", dnsmessage.TypeA))
	if addrs := answerAddrs(got[7]); !slices.Equal(addrs, []netip.Addr{public4}) {
		t.Errorf("answers = %v, want only the public address", addrs)
	}
}
