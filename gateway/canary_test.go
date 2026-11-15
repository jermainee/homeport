package gateway_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/jermainee/homeport/gateway"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

const canaryZone = "canary.homeport.test."

// canaryAnswer is what every name in canaryZone resolves to.
var canaryAnswer = []netip.Addr{netip.MustParseAddr("11.22.33.44"), netip.MustParseAddr("11.22.33.45")}

// zoneResolver is the gateway's own resolver: every name under the zone
// resolves to answer, unless fail is set.
type zoneResolver struct {
	mu     sync.Mutex
	answer []netip.Addr
	fail   bool
}

func (r *zoneResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail || network != "ip4" || !strings.HasSuffix(host, "."+canaryZone) {
		return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
	}
	return append([]netip.Addr(nil), r.answer...), nil
}

// lineAgent is how a fake agent treats the canary's flows: page rewrites
// the HTTP response, dns answers DNS queries.
type lineAgent struct {
	page func([]byte) []byte
	dns  func(q dnsmessage.Question) (dnsmessage.RCode, []netip.Addr)
	// refuse, if set, is asked before each TCP flow; true refuses it.
	refuse func() bool
}

// honest relays HTTP unchanged and answers DNS as the zone does.
var honest = lineAgent{
	page: func(b []byte) []byte { return b },
	dns: func(dnsmessage.Question) (dnsmessage.RCode, []netip.Addr) {
		return dnsmessage.RCodeSuccess, canaryAnswer
	},
}

// handle serves one flow: TCP is relayed to its destination (the canary
// page, on loopback) through page, DNS frames are answered with dns.
func (a lineAgent) handle(open relayprotocol.FlowOpen, s *quic.Stream) {
	switch open.Kind {
	case relayprotocol.KindTCP:
		if a.refuse != nil && a.refuse() {
			_ = relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: relayprotocol.StatusRefused})
			_ = s.Close()
			return
		}
		c, err := net.Dial("tcp", open.Dst.String())
		if err != nil {
			_ = relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: relayprotocol.StatusRefused})
			_ = s.Close()
			return
		}
		defer func() { _ = c.Close() }()
		if err := relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: relayprotocol.StatusOK}); err != nil {
			return
		}
		go func() { _, _ = io.Copy(c, s) }()
		resp, _ := io.ReadAll(c)
		_, _ = s.Write(a.page(resp))
		_ = s.Close()
	case relayprotocol.KindDNS:
		if err := relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: relayprotocol.StatusOK}); err != nil {
			return
		}
		for {
			frame, err := relayprotocol.ReadUDPFrame(s, nil)
			if err != nil {
				_ = s.Close()
				return
			}
			if resp := answerDNS(frame, a.dns); resp != nil {
				_ = relayprotocol.WriteUDPFrame(s, resp)
			}
		}
	default:
		_ = relayprotocol.WriteFlowResult(s, relayprotocol.FlowResult{Status: relayprotocol.StatusRefused})
		_ = s.Close()
	}
}

func answerDNS(query []byte, answer func(dnsmessage.Question) (dnsmessage.RCode, []netip.Addr)) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	rcode, addrs := answer(q)
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionDesired: true, RecursionAvailable: true, RCode: rcode})
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	for _, a := range addrs {
		_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: a.As4()})
	}
	msg, _ := b.Finish()
	return msg
}

// canaryFixture is a gateway that checks its lines every 50 ms, with one
// line whose agent behaves as agent does.
type canaryFixture struct {
	gw       *gateway.Server
	events   *lineEvents
	resolver *zoneResolver
	cert     tls.Certificate
	agent    *fakeAgent
}

func newCanaryFixture(t *testing.T, agent lineAgent, configure ...func(*gateway.CanaryConfig)) *canaryFixture {
	t.Helper()
	gwCert, err := relayprotocol.GenerateSelfSigned("gateway")
	if err != nil {
		t.Fatal(err)
	}
	events := &lineEvents{}
	resolver := &zoneResolver{answer: canaryAnswer}
	canary := &gateway.CanaryConfig{
		HTTPListen: "127.0.0.1:0",
		DNSZone:    canaryZone,
		Resolver:   resolver,
		Interval:   50 * time.Millisecond,
	}
	for _, c := range configure {
		c(canary)
	}
	gw := newGatewayWith(t, gateway.Config{QUICListen: "127.0.0.1:0", Certificate: gwCert, OnLineEvent: events.add, Canary: canary})
	cert, pin := newAgentCert(t)
	if err := gw.AddLine(gateway.Line{ID: "line-1", OwnerMemberID: "member-owner", AgentPin: pin}); err != nil {
		t.Fatalf("add line: %v", err)
	}
	a, err := startAgent(t, gw, cert, agent.handle)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	return &canaryFixture{gw: gw, events: events, resolver: resolver, cert: cert, agent: a}
}

// tampered returns the line's tamper events so far.
func (f *canaryFixture) tampered() []gateway.LineEvent {
	var out []gateway.LineEvent
	for _, e := range f.events.all() {
		if e.Tampered != "" {
			out = append(out, e)
		}
	}
	return out
}

// checkedFor collects the canary's flows to the agent for d.
func (f *canaryFixture) checkedFor(d time.Duration) []relayprotocol.FlowOpen {
	var opens []relayprotocol.FlowOpen
	deadline := time.After(d)
	for {
		select {
		case o := <-f.agent.opens:
			opens = append(opens, o)
		case <-deadline:
			return opens
		}
	}
}

func TestCanaryLeavesAnHonestLineAlone(t *testing.T) {
	f := newCanaryFixture(t, honest)

	opens := f.checkedFor(time.Second)
	var http, dns int
	for _, o := range opens {
		switch o.Kind {
		case relayprotocol.KindTCP:
			http++
		case relayprotocol.KindDNS:
			dns++
		}
	}
	if http < 2 || dns < 2 {
		t.Errorf("the line was checked %d times over HTTP and %d times over DNS in a second; want repeated checks of both", http, dns)
	}
	if got := f.tampered(); len(got) != 0 {
		t.Errorf("an honest line was reported as tampered: %+v", got)
	}
	if !f.gw.LineOnline("line-1") {
		t.Error("an honest line went offline")
	}
}

func TestCanaryPausesALineThatChangesThePage(t *testing.T) {
	agent := honest
	agent.page = func(b []byte) []byte {
		return bytes.Replace(b, []byte("</body>"), []byte("<script src=//ads.example/x.js></script></body>"), 1)
	}
	f := newCanaryFixture(t, agent)

	waitUntil(t, func() bool { return len(f.tampered()) > 0 })
	if got := f.tampered()[0]; got.LineID != "line-1" || got.Tampered != "http" || got.Online {
		t.Errorf("tamper event %+v, want line-1 offline with check http", got)
	}
	assertSuspended(t, f)
}

func TestCanaryPausesALineThatForgesDNSAnswers(t *testing.T) {
	agent := honest
	agent.dns = func(dnsmessage.Question) (dnsmessage.RCode, []netip.Addr) {
		return dnsmessage.RCodeSuccess, []netip.Addr{netip.MustParseAddr("11.99.99.99")}
	}
	f := newCanaryFixture(t, agent)

	waitUntil(t, func() bool { return len(f.tampered()) > 0 })
	if got := f.tampered()[0]; got.Tampered != "dns" {
		t.Errorf("tamper event %+v, want check dns", got)
	}
	assertSuspended(t, f)
}

func TestCanaryPausesALineThatDeniesTheZoneExists(t *testing.T) {
	agent := honest
	agent.dns = func(dnsmessage.Question) (dnsmessage.RCode, []netip.Addr) { return dnsmessage.RCodeNameError, nil }
	f := newCanaryFixture(t, agent)

	waitUntil(t, func() bool { return len(f.tampered()) > 0 })
	if got := f.tampered()[0]; got.Tampered != "dns" {
		t.Errorf("tamper event %+v, want check dns", got)
	}
}

// assertSuspended checks that the line is offline, its agent disconnected,
// and that neither its agent nor the config can bring it back.
func assertSuspended(t *testing.T, f *canaryFixture) {
	t.Helper()
	waitClosed(t, f.agent.conn)
	if f.gw.LineOnline("line-1") {
		t.Error("the tampered line is still online")
	}
	if _, err := startAgent(t, f.gw, f.cert, honest.handle); err == nil {
		t.Error("the tampered line's agent was let back in")
	}
	// The config may list the line until the API has paused it. The line
	// is accepted, so the config applies, but it is not served.
	cert, pin := newAgentCert(t)
	if err := f.gw.AddLine(gateway.Line{ID: "line-1", OwnerMemberID: "member-owner", AgentPin: pin}); err != nil {
		t.Errorf("adding the suspended line again: %v", err)
	}
	if _, err := startAgent(t, f.gw, cert, honest.handle); err == nil {
		t.Error("an agent for the suspended line got in after the config listed it again")
	}
}

func TestCanaryDoesNotPauseALineThatOnlyFails(t *testing.T) {
	// A line that refuses the canary's flows, times out or answers SERVFAIL
	// is broken, not proven to be tampering.
	f := newCanaryFixture(t, lineAgent{
		page: func([]byte) []byte { return nil }, // the connection ends without a response
		dns: func(dnsmessage.Question) (dnsmessage.RCode, []netip.Addr) {
			return dnsmessage.RCodeServerFailure, nil
		},
	})

	if opens := f.checkedFor(time.Second); len(opens) < 4 {
		t.Fatalf("the line was checked %d times in a second", len(opens))
	}
	if got := f.tampered(); len(got) != 0 {
		t.Errorf("a failing line was reported as tampered: %+v", got)
	}
	if !f.gw.LineOnline("line-1") {
		t.Error("a failing line went offline")
	}
}

func TestCanaryPausesNothingWhileItsOwnControlCheckFails(t *testing.T) {
	// Every line would look tampered if the zone were misconfigured, or if
	// the canary page were not where the gateway thinks: the gateway checks
	// both directly before it trusts a line's answer.
	agent := lineAgent{
		page: func([]byte) []byte { return []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nother") },
		dns: func(dnsmessage.Question) (dnsmessage.RCode, []netip.Addr) {
			return dnsmessage.RCodeSuccess, []netip.Addr{netip.MustParseAddr("11.99.99.99")}
		},
	}
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	go func() {
		for {
			c, err := other.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nother"))
			_ = c.Close()
		}
	}()
	f := newCanaryFixture(t, agent, func(c *gateway.CanaryConfig) {
		// The page's public address points at someone else's server.
		c.HTTPTarget = netip.MustParseAddrPort(other.Addr().String())
	})
	f.resolver.mu.Lock()
	f.resolver.fail = true
	f.resolver.mu.Unlock()

	f.checkedFor(500 * time.Millisecond)
	if got := f.tampered(); len(got) != 0 {
		t.Errorf("lines were reported as tampered while the gateway's own checks failed: %+v", got)
	}
	if !f.gw.LineOnline("line-1") {
		t.Error("the line went offline")
	}
}

func TestCanaryPageIsServedOverPlainHTTP(t *testing.T) {
	f := newCanaryFixture(t, honest)
	addr := f.gw.CanaryAddr()
	if !addr.IsValid() {
		t.Fatal("no canary page address")
	}
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "GET /homeport-canary/0123abcd HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	resp, err := io.ReadAll(c)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Contains(resp, []byte("200 OK")) || !bytes.Contains(resp, []byte("0123abcd")) || !bytes.Contains(resp, []byte("text/html")) {
		t.Errorf("canary page response:\n%s", resp)
	}
}

func TestCanaryRemembersAMismatchItCouldNotConfirm(t *testing.T) {
	// The agent changes the page, then refuses the confirming fetch, as an
	// agent that disconnects right after tampering makes it fail. The line
	// stays suspect, and its next changed page suspends it.
	var mu sync.Mutex
	fetches := 0
	agent := honest
	agent.page = func(b []byte) []byte { return bytes.Replace(b, []byte("<p>"), []byte("<p>ad "), 1) }
	agent.refuse = func() bool {
		mu.Lock()
		defer mu.Unlock()
		fetches++
		return fetches == 2
	}
	f := newCanaryFixture(t, agent)

	waitUntil(t, func() bool { return len(f.tampered()) > 0 })
	mu.Lock()
	n := fetches
	mu.Unlock()
	if n < 3 {
		t.Errorf("suspended after %d fetches; want the refused confirmation, then another changed page", n)
	}
	if got := f.tampered()[0]; got.Tampered != "http" || !got.ObservedIP.IsValid() || got.AgentVersion != "test" {
		t.Errorf("tamper event %+v; want check http with the agent's address and version", got)
	}
}

func TestCanaryAcceptsAnAnswerTheAgentTrimmed(t *testing.T) {
	// An agent keeps an answer within 512 bytes and drops addresses its
	// policy refuses, so an honest line may answer with fewer addresses.
	agent := honest
	agent.dns = func(dnsmessage.Question) (dnsmessage.RCode, []netip.Addr) {
		return dnsmessage.RCodeSuccess, canaryAnswer[:1]
	}
	f := newCanaryFixture(t, agent)

	if opens := f.checkedFor(time.Second); len(opens) < 4 {
		t.Fatalf("the line was checked %d times in a second", len(opens))
	}
	if got := f.tampered(); len(got) != 0 {
		t.Errorf("a line that trimmed an answer was reported tampered: %+v", got)
	}
}

func TestCanaryConfirmsAMismatchBeforeItSuspends(t *testing.T) {
	// One odd answer, then honest ones: the line is not suspended.
	var mu sync.Mutex
	odd := 1
	agent := honest
	agent.dns = func(dnsmessage.Question) (dnsmessage.RCode, []netip.Addr) {
		mu.Lock()
		defer mu.Unlock()
		if odd > 0 {
			odd--
			return dnsmessage.RCodeSuccess, []netip.Addr{netip.MustParseAddr("11.99.99.99")}
		}
		return dnsmessage.RCodeSuccess, canaryAnswer
	}
	f := newCanaryFixture(t, agent)

	f.checkedFor(time.Second)
	if got := f.tampered(); len(got) != 0 {
		t.Errorf("a single odd answer suspended the line: %+v", got)
	}
}

func TestCanaryStopsSuspendingWhenManyLinesFailAtOnce(t *testing.T) {
	// Many lines failing together points at Homeport (a changed zone, a
	// broken page), not at their owners: the canary stops suspending.
	f := newCanaryFixture(t, honest)
	forged := honest
	forged.dns = func(dnsmessage.Question) (dnsmessage.RCode, []netip.Addr) {
		return dnsmessage.RCodeSuccess, []netip.Addr{netip.MustParseAddr("11.99.99.99")}
	}
	for i := range 8 {
		cert, pin := newAgentCert(t)
		id := "line-forged-" + strconv.Itoa(i)
		if err := f.gw.AddLine(gateway.Line{ID: id, OwnerMemberID: "member-owner", AgentPin: pin}); err != nil {
			t.Fatal(err)
		}
		if _, err := startAgent(t, f.gw, cert, forged.handle); err != nil {
			t.Fatal(err)
		}
	}

	waitUntil(t, func() bool { return len(f.tampered()) >= gateway.CanaryMaxSuspensions })
	f.checkedFor(time.Second)
	if n := len(f.tampered()); n != gateway.CanaryMaxSuspensions {
		t.Errorf("%d lines were suspended, want the first %d only", n, gateway.CanaryMaxSuspensions)
	}
	if !f.gw.LineOnline("line-1") {
		t.Error("the honest line went offline")
	}
}

// sharedResolver answers every name with the same slice, as a resolver with
// a cache may: the canary must not change it.
type sharedResolver struct{ answer []netip.Addr }

func (r sharedResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r.answer, nil
}

func TestCanaryLeavesTheResolversAnswerAlone(t *testing.T) {
	mapped := make([]netip.Addr, len(canaryAnswer))
	for i, a := range canaryAnswer {
		mapped[i] = netip.AddrFrom16(a.As16()) // ::ffff:a.b.c.d
	}
	shared := sharedResolver{answer: mapped}
	f := newCanaryFixture(t, honest, func(c *gateway.CanaryConfig) { c.Resolver = shared })

	if opens := f.checkedFor(500 * time.Millisecond); len(opens) < 2 {
		t.Fatalf("the line was checked %d times", len(opens))
	}
	for i, a := range shared.answer {
		if !a.Is4In6() {
			t.Fatalf("the canary changed the resolver's answer: address %d is %s now", i, a)
		}
	}
	if got := f.tampered(); len(got) != 0 {
		t.Errorf("an honest line was reported tampered: %+v", got)
	}
}
