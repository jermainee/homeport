package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/net/dns/dnsmessage"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// virtualResolver is the address clients send DNS to; the agent never
// dials it.
var virtualResolver = netip.MustParseAddrPort("10.77.0.1:53")

// fakeResolver answers lookups from a table keyed by network and name, and
// records every lookup.
type fakeResolver struct {
	answers map[string][]netip.Addr // "ip4 example.com." → addresses

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
	return slices.Contains(r.lookups, key)
}

// startDNS runs an agent with res and opens a DNS flow on it.
func startDNS(t *testing.T, res *fakeResolver) *quic.Stream {
	t.Helper()
	g := newFakeGateway(t)
	startAgent(t, g, func(c *Config) { c.Resolver = res })
	return g.nextConn(t).mustDial(t, relayprotocol.KindDNS, virtualResolver)
}

// query builds a DNS query with id for name and type.
func query(t *testing.T, id uint16, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	q := dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}
	if err := b.Question(q); err != nil {
		t.Fatal(err)
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// exchange sends queries on s and returns the responses by query ID.
func exchange(t *testing.T, s *quic.Stream, queries ...[]byte) map[uint16]dnsmessage.Message {
	t.Helper()
	for _, q := range queries {
		if err := relayprotocol.WriteUDPFrame(s, q); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.SetReadDeadline(time.Now().Add(waitFor))
	defer func() { _ = s.SetReadDeadline(time.Time{}) }()
	out := make(map[uint16]dnsmessage.Message)
	for range queries {
		frame, err := relayprotocol.ReadUDPFrame(s, nil)
		if err != nil {
			t.Fatalf("read DNS response: %v", err)
		}
		var m dnsmessage.Message
		if err := m.Unpack(frame); err != nil {
			t.Fatalf("unpack DNS response: %v", err)
		}
		out[m.ID] = m
	}
	return out
}

// answerAddrs returns the addresses in m's A and AAAA answers.
func answerAddrs(m dnsmessage.Message) []netip.Addr {
	var out []netip.Addr
	for _, rr := range m.Answers {
		switch body := rr.Body.(type) {
		case *dnsmessage.AResource:
			out = append(out, netip.AddrFrom4(body.A))
		case *dnsmessage.AAAAResource:
			out = append(out, netip.AddrFrom16(body.AAAA))
		}
	}
	return out
}

func TestDNSQueriesAreAnsweredThroughTheResolver(t *testing.T) {
	v4 := netip.MustParseAddr("11.22.33.44")
	v6 := netip.MustParseAddr("2a00:1450:4001::1")
	res := &fakeResolver{answers: map[string][]netip.Addr{
		"ip4 origin.homeport.test.": {v4},
		"ip6 origin.homeport.test.": {v6},
	}}
	s := startDNS(t, res)

	// Both at once, as stub resolvers send them; each frame is answered.
	got := exchange(t, s,
		query(t, 0x1111, "origin.homeport.test.", dnsmessage.TypeA),
		query(t, 0x2222, "origin.homeport.test.", dnsmessage.TypeAAAA),
	)
	for id, want := range map[uint16]netip.Addr{0x1111: v4, 0x2222: v6} {
		m, ok := got[id]
		if !ok {
			t.Fatalf("no response with ID %#x", id)
		}
		if !m.Response || m.RCode != dnsmessage.RCodeSuccess || !m.RecursionAvailable {
			t.Fatalf("response %#x header = %+v, want a successful recursive response", id, m.Header)
		}
		if len(m.Questions) != 1 || m.Questions[0].Name.String() != "origin.homeport.test." {
			t.Fatalf("response %#x questions = %v, want the query's question", id, m.Questions)
		}
		if addrs := answerAddrs(m); !slices.Equal(addrs, []netip.Addr{want}) {
			t.Fatalf("response %#x answers = %v, want %v", id, addrs, want)
		}
	}
	if !res.looked("ip4 origin.homeport.test.") || !res.looked("ip6 origin.homeport.test.") {
		t.Fatalf("lookups = %v, want an ip4 and an ip6 lookup of the absolute name", res.lookups)
	}
}

func TestDNSAnswersNeverRevealHomeNetworkAddresses(t *testing.T) {
	res := &fakeResolver{answers: map[string][]netip.Addr{
		"ip4 mixed.homeport.test.":  {netip.MustParseAddr("192.168.178.20"), netip.MustParseAddr("11.22.33.44")},
		"ip4 rebind.homeport.test.": {netip.MustParseAddr("192.168.178.1")},
		"ip6 rebind.homeport.test.": {netip.MustParseAddr("fd00::1")},
		"ip4 myhome.dyndns.test.":   {testObservedIP},
	}}
	s := startDNS(t, res)

	got := exchange(t, s,
		query(t, 1, "mixed.homeport.test.", dnsmessage.TypeA),
		query(t, 2, "rebind.homeport.test.", dnsmessage.TypeA),
		query(t, 3, "rebind.homeport.test.", dnsmessage.TypeAAAA),
		query(t, 4, "myhome.dyndns.test.", dnsmessage.TypeA),
	)
	if addrs := answerAddrs(got[1]); !slices.Equal(addrs, []netip.Addr{netip.MustParseAddr("11.22.33.44")}) {
		t.Fatalf("mixed answers = %v, want only the public address", addrs)
	}
	for id := uint16(2); id <= 4; id++ {
		if m := got[id]; m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
			t.Fatalf("response %d = %v with %d answers, want success without answers", id, m.RCode, len(m.Answers))
		}
	}
}

func TestDNSNamesThatOnlyExistAtHomeAreNeverLookedUp(t *testing.T) {
	// Looking these up would send mDNS or router queries into the home
	// network, and their timing would reveal which devices exist there.
	res := &fakeResolver{}
	s := startDNS(t, res)
	names := []string{
		"iphone.local.", "LOCAL.", "localhost.", "db.localhost.", "nas.home.arpa.",
		"printer.lan.", "router.home.", "box.internal.", "fritz.box.", "host.localdomain.",
		"nas.", // a single label: the router answers those for LAN hosts
	}
	var queries [][]byte
	for i, name := range names {
		queries = append(queries, query(t, uint16(i+1), name, dnsmessage.TypeA))
	}
	got := exchange(t, s, queries...)
	for i, name := range names {
		if m := got[uint16(i+1)]; m.RCode != dnsmessage.RCodeNameError || len(m.Answers) != 0 {
			t.Errorf("%s: %v with %d answers, want NXDOMAIN", name, m.RCode, len(m.Answers))
		}
	}
	res.mu.Lock()
	defer res.mu.Unlock()
	if len(res.lookups) != 0 {
		t.Fatalf("looked up %v, want no lookups", res.lookups)
	}
}

func TestDNSWithoutAddressesIsAnEmptyAnswer(t *testing.T) {
	// A name with only AAAA records makes an ip4 lookup fail as "not
	// found". NXDOMAIN would tell the client the name does not exist at
	// all, so the agent answers "no records of this type" instead.
	s := startDNS(t, &fakeResolver{})
	m := exchange(t, s, query(t, 7, "v6only.homeport.test.", dnsmessage.TypeA))[7]
	if m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Fatalf("response = %v with %d answers, want success without answers", m.RCode, len(m.Answers))
	}
}

func TestDNSRecordTypesOtherThanAddresses(t *testing.T) {
	s := startDNS(t, &fakeResolver{})
	got := exchange(t, s,
		// Browsers ask for HTTPS records next to every address lookup;
		// "none" lets them go on with A and AAAA right away.
		query(t, 1, "origin.homeport.test.", dnsmessage.TypeHTTPS),
		query(t, 2, "origin.homeport.test.", dnsmessage.TypeTXT),
	)
	if m := got[1]; m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Fatalf("HTTPS response = %v with %d answers, want success without answers", m.RCode, len(m.Answers))
	}
	if m := got[2]; m.RCode != dnsmessage.RCodeNotImplemented {
		t.Fatalf("TXT response = %v, want NOTIMP", m.RCode)
	}
}

func TestDNSFlowEndsAfterItsLastAnswer(t *testing.T) {
	res := &fakeResolver{answers: map[string][]netip.Addr{
		"ip4 origin.homeport.test.": {netip.MustParseAddr("11.22.33.44")},
	}}
	s := startDNS(t, res)
	if err := relayprotocol.WriteUDPFrame(s, query(t, 9, "origin.homeport.test.", dnsmessage.TypeA)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_ = s.SetReadDeadline(time.Now().Add(waitFor))
	if _, err := relayprotocol.ReadUDPFrame(s, nil); err != nil {
		t.Fatalf("answer to the last query: %v", err)
	}
	if _, err := relayprotocol.ReadUDPFrame(s, nil); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last answer: %v, want the end of the stream", err)
	}
}
