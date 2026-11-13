//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jermainee/homeport/agent"
	"github.com/jermainee/homeport/e2e/internal/fakeapi"
	"github.com/jermainee/homeport/e2e/internal/wgclient"
	"github.com/jermainee/homeport/gateway"
	"github.com/jermainee/homeport/gateway/service"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// The control API's view of the world: member B uses member A's line
// through peer 10.77.0.2.
const (
	gatewayToken = "hpgw_e2e.secret"                      // #nosec G101 -- the fake API's test token
	userMember   = "01a1c4af-8795-7a28-9249-adbd2ba1c04a" // B
	ownerMember  = "01a1c4af-897b-7f0a-a915-c1ba8ba7b153" // A
	peerID       = "01a1c4af-89c1-7c5b-a49a-3d557fc869e7"
	lineID       = "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e6"
	topUpHost    = "homeport.example"
	originName   = "origin.homeport.test"
)

var (
	peerIP          = netip.MustParseAddr("10.77.0.2")
	virtualResolver = netip.MustParseAddr("10.77.0.1")

	// The client sends its flows to public-looking destinations, as a
	// member would. A real gateway drops packets addressed to loopback, so
	// the agent's test routes send these to the loopback test servers.
	tcpDst = netip.MustParseAddrPort("203.0.113.10:80")
	udpDst = netip.MustParseAddrPort("203.0.113.10:7")
)

// waitFor bounds every wait for something that should happen soon.
const waitFor = 15 * time.Second

// transferTimeout bounds one transfer. The race detector slows the tunnel
// down to a few hundred kB/s.
const transferTimeout = 3 * time.Minute

// stack is one Homeport deployment in a single process: an origin on the
// internet, the control API, a gateway, member A's agent and member B's
// WireGuard client.
type stack struct {
	t        *testing.T
	origin   *origin
	api      *fakeapi.API
	apiURL   string
	resolver *fakeResolver

	gwKey      gateway.Key
	gwCert     tls.Certificate
	wgPort     uint16
	quicListen string
	gw         *service.Service
	stopGW     func()
	gwLogs     *syncBuffer

	agent     *agent.Agent
	agentLogs *syncBuffer

	clientKey gateway.Key
	client    *wgclient.Client
}

type stackOptions struct {
	leases fakeapi.LeasePolicy
}

func withLeases(p fakeapi.LeasePolicy) func(*stackOptions) {
	return func(o *stackOptions) { o.leases = p }
}

// newStack starts everything and returns once the agent serves the line
// and the client is up. Everything stops when the test ends; then the
// stack checks the API saw no contract violation and the logs name no
// destination.
func newStack(t *testing.T, opts ...func(*stackOptions)) *stack {
	t.Helper()
	var o stackOptions
	for _, opt := range opts {
		opt(&o)
	}
	s := &stack{t: t, origin: startOrigin(t), gwLogs: &syncBuffer{}, agentLogs: &syncBuffer{}}
	s.resolver = &fakeResolver{answers: map[string][]netip.Addr{"ip4 " + originName + ".": {tcpDst.Addr()}}}

	agentCert, err := relayprotocol.GenerateSelfSigned("homeport-agent")
	if err != nil {
		t.Fatal(err)
	}
	agentPin := relayprotocol.PinOf(agentCert.Leaf)
	if s.clientKey, err = gateway.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	s.api = fakeapi.New(gatewayToken, fakeapi.Config{
		ETag:           `"cfg-1"`,
		TopUpHostnames: []string{topUpHost},
		Peers: []fakeapi.Peer{{
			PeerID:    peerID,
			MemberID:  userMember,
			PublicKey: s.clientKey.PublicKey().String(),
			IPv4:      peerIP.String(),
			IPv6:      "fd77:6870::2",
			LineID:    lineID,
		}},
		Lines: []fakeapi.Line{{
			LineID:           lineID,
			OwnerMemberID:    ownerMember,
			DeviceSPKISHA256: hex.EncodeToString(agentPin[:]),
		}},
	})
	if o.leases != nil {
		s.api.SetLeases(o.leases)
	}
	apiSrv := httptest.NewServer(s.api)
	s.apiURL = apiSrv.URL
	t.Cleanup(func() {
		apiSrv.Close()
		if v := s.api.Violations(); len(v) > 0 {
			t.Errorf("the gateway broke the control API contract:\n%s", strings.Join(v, "\n"))
		}
		s.assertLogsNameNoDestination()
	})

	if s.gwKey, err = gateway.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	if s.gwCert, err = relayprotocol.GenerateSelfSigned("gateway"); err != nil {
		t.Fatal(err)
	}
	s.wgPort = freeUDPPort(t)
	s.quicListen = "127.0.0.1:0"
	s.startGateway()

	// The agent learns the gateway's QUIC endpoint and pin the way the
	// app does: from what the gateway announced to the API.
	self, _ := s.api.Self()
	pinBytes, err := hex.DecodeString(self.QUICSPKISHA256)
	if err != nil || len(pinBytes) != len(relayprotocol.Pin{}) {
		t.Fatalf("announced pin %q", self.QUICSPKISHA256)
	}
	cfg := agent.Config{
		GatewayAddr:   self.QUICEndpoint,
		GatewayPin:    relayprotocol.Pin(pinBytes),
		Certificate:   agentCert,
		Resolver:      s.resolver,
		Logger:        slog.New(slog.NewJSONHandler(s.agentLogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		StatsInterval: 200 * time.Millisecond,
		ReconnectMin:  50 * time.Millisecond,
		ReconnectMax:  500 * time.Millisecond,
		TunnelCheck:   func() (bool, error) { return false, nil },
	}
	agent.RouteTestOrigin(&cfg, tcpDst, s.origin.tcp)
	agent.RouteTestOrigin(&cfg, udpDst, s.origin.udp)
	if s.agent, err = agent.New(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.agent.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("agent: %v", err)
			}
		case <-time.After(waitFor):
			t.Error("the agent did not stop")
		}
	})
	s.waitLineOnline()
	s.newClient()
	return s
}

// startGateway runs the gateway from the API, as cmd/homeport-gateway
// does, and waits until it has announced itself.
func (s *stack) startGateway() {
	t := s.t
	t.Helper()
	before := len(s.api.Calls())
	// After a stop, quic-go keeps the QUIC port through the connections'
	// draining period (RFC 9000, 10.2), so a restart may have to wait.
	var svc *service.Service
	var err error
	deadline := time.Now().Add(waitFor)
	for {
		svc, err = s.newGateway()
		if !errors.Is(err, syscall.EADDRINUSE) || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	s.gw = svc
	s.quicListen = svc.Server().QUICAddr().String() // a restart listens there again
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	var once sync.Once
	s.stopGW = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("gateway: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Error("the gateway did not stop")
			}
		})
	}
	t.Cleanup(s.stopGW)
	waitUntil(t, "the gateway announced itself", func() bool {
		for _, c := range s.api.Calls()[before:] {
			if c.Path == fakeapi.PathSelf && c.Status == http.StatusNoContent {
				return true
			}
		}
		return false
	})
}

func (s *stack) newGateway() (*service.Service, error) {
	return service.New(service.Config{
		API:          s.apiURL,
		Token:        gatewayToken,
		PrivateKey:   s.gwKey,
		WGListenPort: s.wgPort,
		WGEndpoint:   net.JoinHostPort("127.0.0.1", strconv.Itoa(int(s.wgPort))),
		QUICListen:   s.quicListen,
		Certificate:  s.gwCert,
		Version:      "0.1.0",
		Logger:       slog.New(slog.NewJSONHandler(s.gwLogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
}

// stopGateway shuts the gateway down the way SIGTERM does: final usage
// events, lease returns, then its agents go offline. It returns once all of
// that is reported.
func (s *stack) stopGateway() { s.stopGW() }

func (s *stack) waitLineOnline() {
	s.t.Helper()
	waitUntil(s.t, "the agent serves the line", func() bool {
		return s.gw.Server().LineOnline(lineID) && s.agent.Stats().State == agent.StateConnected
	})
}

// newClient starts member B's WireGuard client with the config the API
// would hand out, replacing any earlier client.
func (s *stack) newClient() {
	t := s.t
	t.Helper()
	if s.client != nil {
		s.client.Close()
	}
	self, _ := s.api.Self()
	c, err := wgclient.New(wgclient.Config{
		PrivateKey:    [32]byte(s.clientKey),
		Address:       peerIP,
		DNS:           virtualResolver,
		Endpoint:      self.WGEndpoint,
		PeerPublicKey: self.WGPublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.client = c
	t.Cleanup(c.Close)
}

// meter is the gateway's count for the peer.
func (s *stack) meter() gateway.Usage { return s.gw.Server().PeerUsage(peerIP) }

// counts is what the gateway metered and what the client's tun counted.
type counts struct {
	gateway        gateway.Usage
	sent, received uint64
}

// quiesce waits until no byte moves for a while, so in-flight packets
// (final ACKs and FINs) have landed, and returns the counters then.
func (s *stack) quiesce() counts {
	s.t.Helper()
	return waitStable(s.t, 500*time.Millisecond, func() counts {
		return counts{s.meter(), s.client.Sent(), s.client.Received()}
	})
}

func (s *stack) httpClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: s.client.Net.DialContext, DisableKeepAlives: true},
	}
}

// get fetches path (under contentPath) from the origin through the tunnel.
func (s *stack) get(path string) []byte {
	s.t.Helper()
	body, err := s.tryGet("http://"+tcpDst.String()+contentPath+path, transferTimeout)
	if err != nil {
		s.t.Fatalf("GET %s: %v", path, err)
	}
	return body
}

// tryGet fetches url through the tunnel and returns what arrived.
func (s *stack) tryGet(url string, timeout time.Duration) ([]byte, error) {
	resp, err := s.httpClient(timeout).Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("status %d", resp.StatusCode)
	}
	return body, err
}

// upload posts body to the origin through the tunnel and returns the
// SHA-256 the origin computed.
func (s *stack) upload(body []byte) string {
	s.t.Helper()
	resp, err := s.httpClient(transferTimeout).Post("http://"+tcpDst.String()+contentPath+"/upload", "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		s.t.Fatalf("upload: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	sum, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		s.t.Fatalf("upload answered %d: %v", resp.StatusCode, err)
	}
	return string(sum)
}

// destinationPatterns are what the logs must never hold: where the
// member's flows went, the names they looked up, and their content.
func (s *stack) destinationPatterns() []*regexp.Regexp {
	pats := []string{
		regexp.QuoteMeta(tcpDst.Addr().String()),
		regexp.QuoteMeta("10.0.0.1"),
		regexp.QuoteMeta("192.168.1.1"),
		regexp.QuoteMeta(originName),
		regexp.QuoteMeta(contentPath),
	}
	// The loopback origins' ports, in any address form or as a port field.
	for _, ap := range []netip.AddrPort{s.origin.tcp, s.origin.udp} {
		port := strconv.Itoa(int(ap.Port()))
		pats = append(pats, `[0-9.\]]:`+port+`\b`, `"(dst_)?port":"?`+port+`\b`)
	}
	out := make([]*regexp.Regexp, len(pats))
	for i, p := range pats {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

func (s *stack) assertLogsNameNoDestination() {
	s.t.Helper()
	for name, logs := range map[string]string{"gateway": s.gwLogs.String(), "agent": s.agentLogs.String()} {
		for _, re := range s.destinationPatterns() {
			if loc := re.FindStringIndex(logs); loc != nil {
				start, end := max(loc[0]-200, 0), min(loc[1]+200, len(logs))
				s.t.Errorf("the %s's log names a destination (%s):\n%s", name, re, logs[start:end])
			}
		}
	}
}

// contentPath prefixes every origin path, so a test can tell that no
// request content reached the logs.
const contentPath = "/homeport-e2e-content"

// origin is a server on the internet: HTTP over TCP and a UDP echo, both on
// loopback.
type origin struct {
	tcp, udp netip.AddrPort
}

func startOrigin(t *testing.T) *origin {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+contentPath+"/blob/{size}", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.PathValue("size"))
		if err != nil {
			http.Error(w, "bad size", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(blob(n))
	})
	mux.HandleFunc("POST "+contentPath+"/upload", func(w http.ResponseWriter, r *http.Request) {
		h := sha256.New()
		if _, err := io.Copy(h, r.Body); err != nil {
			return
		}
		_, _ = io.WriteString(w, hex.EncodeToString(h.Sum(nil)))
	})
	// /slow/{size}/{rate} sends size bytes at about rate bytes per second.
	mux.HandleFunc("GET "+contentPath+"/slow/{size}/{rate}", func(w http.ResponseWriter, r *http.Request) {
		size, err1 := strconv.Atoi(r.PathValue("size"))
		rate, err2 := strconv.Atoi(r.PathValue("rate"))
		if err1 != nil || err2 != nil || rate <= 0 {
			http.Error(w, "bad size or rate", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(size))
		const tick = 20 * time.Millisecond
		chunk := max(rate/int(time.Second/tick), 1)
		body := blob(size)
		next := time.Now()
		for sent := 0; sent < size; {
			n := min(chunk, size-sent)
			if _, err := w.Write(body[sent : sent+n]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			sent += n
			next = next.Add(tick)
			time.Sleep(time.Until(next))
		}
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, from, err := pc.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteToUDPAddrPort(buf[:n], from)
		}
	}()
	return &origin{
		tcp: srv.Listener.Addr().(*net.TCPAddr).AddrPort(),
		udp: pc.LocalAddr().(*net.UDPAddr).AddrPort(),
	}
}

// blob is n pseudo-random bytes, the same for the same n.
func blob(n int) []byte {
	var seed [32]byte
	binary.LittleEndian.PutUint64(seed[:], uint64(n)) // #nosec G115 -- sizes are not negative
	r := rand.NewChaCha8(seed)
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeResolver stands in for the home ISP's resolver behind the agent.
type fakeResolver struct {
	answers map[string][]netip.Addr // "ip4 name." → addresses

	mu      sync.Mutex
	lookups []string
}

func (r *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	key := network + " " + host
	r.mu.Lock()
	r.lookups = append(r.lookups, key)
	r.mu.Unlock()
	if addrs, ok := r.answers[key]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (r *fakeResolver) looked(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lookups {
		if l == key {
			return true
		}
	}
	return false
}

// syncBuffer collects log output from many goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// freeUDPPort returns a UDP port nothing listens on right now.
func freeUDPPort(t *testing.T) uint16 {
	t.Helper()
	pc, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	return pc.LocalAddr().(*net.UDPAddr).AddrPort().Port()
}

// waitUntil polls until cond holds.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitStable polls until read returns the same value for quiet.
func waitStable[T comparable](t *testing.T, quiet time.Duration, read func() T) T {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	last, since := read(), time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		if v := read(); v != last {
			last, since = v, time.Now()
		} else if time.Since(since) >= quiet {
			return v
		}
	}
	t.Fatalf("counters did not settle: %+v", last)
	return last
}
