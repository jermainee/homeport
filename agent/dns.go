package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// Resolver looks up a host's IP addresses, as *net.Resolver does: a name
// without records of the network's family is a *net.DNSError with
// IsNotFound.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

const (
	// dnsTTL is the TTL of every answer. Answers are kept short-lived, so a
	// client asks again soon rather than caching past a change.
	dnsTTL = 60
	// dnsQueryTimeout bounds one lookup.
	dnsQueryTimeout = 5 * time.Second
	// dnsMaxInFlight bounds the lookups one DNS flow runs at once.
	dnsMaxInFlight = 32
	// dnsMaxUDPSize is the largest answer the agent sends to a client
	// that advertises EDNS(0), the size DNS Flag Day 2020 recommends.
	dnsMaxUDPSize = 1232
	// dnsMinUDPSize is the largest answer for a client without EDNS(0).
	dnsMinUDPSize = 512
)

// serveDNS answers a DNS flow: each frame from the gateway is one DNS query,
// and the agent answers each with one frame. Queries run concurrently, so
// answers can come back in another order; DNS IDs match them up. Its
// destination is the gateway's virtual resolver, which the agent never
// dials, so the deny-list does not apply to it. It applies to the answers.
func (a *Agent) serveDNS(ctx context.Context, f *flow, pol *policy, cnt *counters) {
	if !a.answer(f, relayprotocol.StatusOK, "") {
		return
	}
	closed := cnt.flowOpened()
	defer closed()
	stop := context.AfterFunc(ctx, func() { f.cancel(relayprotocol.CodeFlowCancelled) })
	defer stop()

	var (
		wg       sync.WaitGroup
		writeMu  sync.Mutex
		inFlight = make(chan struct{}, dnsMaxInFlight)
	)
	defer wg.Wait()
	for {
		q, err := relayprotocol.ReadUDPFrame(f.stream, nil)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			f.cancel(relayprotocol.CodeProtocolError)
			return
		}
		if err != nil {
			f.cancel(remoteCode(err))
			return
		}
		cnt.bytesOut.Add(uint64(len(q)))
		select {
		case inFlight <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Go(func() {
			defer func() { <-inFlight }()
			resp := a.resolve(ctx, q, pol)
			if resp == nil {
				return // not a query; dropped, as a DNS server would
			}
			writeMu.Lock()
			err := relayprotocol.WriteUDPFrame(f.stream, resp)
			writeMu.Unlock()
			if err == nil {
				cnt.bytesIn.Add(uint64(len(resp)))
			}
		})
	}
	// The gateway sent its last query. Answer what is still open, then end
	// the agent's side; nothing writes once wg is done.
	wg.Wait()
	f.closeSend()
}

// resolve answers one DNS query in wire format through the resolver. It
// answers A and AAAA queries, drops addresses the policy refuses, answers
// HTTPS and SVCB queries with no records and refuses other types with
// NOTIMP. It returns nil for a message that is not a query.
func (a *Agent) resolve(ctx context.Context, query []byte, pol *policy) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response {
		return nil
	}
	reply := dnsmessage.Header{
		ID:                 h.ID,
		Response:           true,
		OpCode:             h.OpCode,
		RecursionDesired:   h.RecursionDesired,
		RecursionAvailable: true,
	}
	if h.OpCode != 0 {
		reply.RCode = dnsmessage.RCodeNotImplemented
		return packDNS(reply, nil, nil, 0)
	}
	questions, err := p.AllQuestions()
	if err != nil || len(questions) != 1 {
		reply.RCode = dnsmessage.RCodeFormatError
		return packDNS(reply, nil, nil, 0)
	}
	q := questions[0]
	edns := ednsSize(&p)

	var addrs []netip.Addr
	switch {
	case q.Class != dnsmessage.ClassINET:
		reply.RCode = dnsmessage.RCodeNotImplemented
	case homeOnlyName(q.Name.String()):
		reply.RCode = dnsmessage.RCodeNameError
	case q.Type == dnsmessage.TypeA || q.Type == dnsmessage.TypeAAAA:
		addrs, reply.RCode = a.lookup(ctx, q, pol)
	case q.Type == dnsmessage.TypeHTTPS || q.Type == dnsmessage.TypeSVCB:
		// No records: clients go on with A and AAAA.
	default:
		reply.RCode = dnsmessage.RCodeNotImplemented
	}
	return packDNS(reply, &q, addrs, edns)
}

// homeZones are names that exist only inside a network, never on the
// internet: mDNS (.local), loopback, RFC 8375's home.arpa, the private-use
// .internal, and zones home routers serve for their LAN.
var homeZones = []string{
	"local", "localhost", "home.arpa", "internal",
	"lan", "home", "localdomain", "fritz.box",
}

// homeOnlyName reports whether name can only resolve inside the home
// network: a single label, which routers answer for LAN hosts, or a name in
// one of homeZones. The agent answers these with NXDOMAIN without a lookup.
// No public resolver knows them, and a resolver on the machine would send
// mDNS or router queries into the home network, whose timing would reveal
// which devices exist there.
func homeOnlyName(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if !strings.Contains(name, ".") {
		return true
	}
	for _, zone := range homeZones {
		if name == zone || strings.HasSuffix(name, "."+zone) {
			return true
		}
	}
	return false
}

// lookup resolves an A or AAAA question and keeps the addresses the policy
// allows, so a client cannot map the home network through DNS.
func (a *Agent) lookup(ctx context.Context, q dnsmessage.Question, pol *policy) ([]netip.Addr, dnsmessage.RCode) {
	network := "ip4"
	if q.Type == dnsmessage.TypeAAAA {
		network = "ip6"
	}
	ctx, cancel := context.WithTimeout(ctx, dnsQueryTimeout)
	defer cancel()
	// The name stays absolute (with its final dot), so the home network's
	// search domains never apply.
	found, err := a.cfg.Resolver.LookupNetIP(ctx, network, q.Name.String())
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			// "Not found" also means "no records of this family", so
			// it is answered as no records rather than NXDOMAIN.
			return nil, dnsmessage.RCodeSuccess
		}
		return nil, dnsmessage.RCodeServerFailure
	}
	addrs := make([]netip.Addr, 0, len(found))
	for _, addr := range found {
		if q.Type == dnsmessage.TypeA && !addr.Unmap().Is4() ||
			q.Type == dnsmessage.TypeAAAA && (!addr.Is6() || addr.Is4In6()) {
			continue
		}
		if pol.allowsAnswer(addr) {
			addrs = append(addrs, addr.Unmap())
		}
	}
	return addrs, dnsmessage.RCodeSuccess
}

// ednsSize returns the UDP payload size the query's EDNS(0) OPT record
// advertises, or 0 if it has none. p must be past the questions.
func ednsSize(p *dnsmessage.Parser) uint16 {
	if p.SkipAllAnswers() != nil || p.SkipAllAuthorities() != nil {
		return 0
	}
	for {
		h, err := p.AdditionalHeader()
		if err != nil {
			return 0
		}
		if h.Type == dnsmessage.TypeOPT {
			return max(uint16(h.Class), dnsMinUDPSize)
		}
		if p.SkipAdditional() != nil {
			return 0
		}
	}
}

// packDNS builds a reply with as many of the answers as fit the client's
// size limit. Leaving answers out of a full address set is what DNS servers
// do too; it needs no truncation flag, which would send the client to DNS
// over TCP for the same answer.
func packDNS(h dnsmessage.Header, q *dnsmessage.Question, answers []netip.Addr, edns uint16) []byte {
	limit := dnsMinUDPSize
	if edns > 0 {
		limit = int(min(edns, dnsMaxUDPSize))
	}
	for n := len(answers); n >= 0; n-- {
		msg, err := buildDNS(h, q, answers[:n], edns > 0)
		if err != nil {
			return nil
		}
		if len(msg) <= limit || n == 0 {
			return msg
		}
	}
	return nil
}

func buildDNS(h dnsmessage.Header, q *dnsmessage.Question, answers []netip.Addr, withOPT bool) ([]byte, error) {
	b := dnsmessage.NewBuilder(make([]byte, 0, dnsMinUDPSize), h)
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if q != nil {
		if err := b.Question(*q); err != nil {
			return nil, err
		}
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	for _, addr := range answers {
		rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: dnsTTL}
		var err error
		if addr.Is4() {
			err = b.AResource(rh, dnsmessage.AResource{A: addr.As4()})
		} else {
			err = b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: addr.As16()})
		}
		if err != nil {
			return nil, err
		}
	}
	if withOPT {
		if err := b.StartAdditionals(); err != nil {
			return nil, err
		}
		var rh dnsmessage.ResourceHeader
		if err := rh.SetEDNS0(dnsMaxUDPSize, dnsmessage.RCodeSuccess, false); err != nil {
			return nil, err
		}
		if err := b.OPTResource(rh, dnsmessage.OPTResource{}); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}
