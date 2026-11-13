package agent

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// deniedDestinations holds a destination for every range and port the policy
// refuses ("Seller home network protection" in docs/concept.md), with the
// edges of each range. None of them is ever dialed: the policy refuses them
// in the dialer's Control hook, before connect.
var deniedDestinations = []struct {
	name string
	dst  string
}{
	{"this network, which reaches the machine itself", "0.0.0.0:80"},
	{"10/8 first", "10.0.0.1:443"},
	{"10/8 last", "10.255.255.254:443"},
	{"100.64/10 first", "100.64.0.1:443"},
	{"100.64/10 last", "100.127.255.254:443"},
	{"127/8 on another port than the test origin", "127.0.0.1:1"},
	{"127/8 last", "127.255.255.254:443"},
	{"169.254/16, cloud metadata", "169.254.169.254:80"},
	{"172.16/12 first", "172.16.0.1:443"},
	{"172.16/12 last", "172.31.255.254:443"},
	{"192.0.0/24 protocol assignments", "192.0.0.170:443"},
	{"192.0.2/24 documentation", "192.0.2.1:443"},
	{"192.168/16 first", "192.168.0.1:80"},
	{"192.168/16 home router", "192.168.178.1:80"},
	{"192.168/16 last", "192.168.255.255:80"},
	{"198.18/15 benchmarking and fake-IP proxies", "198.19.255.254:443"},
	{"198.51.100/24 documentation", "198.51.100.1:443"},
	{"203.0.113/24 documentation", "203.0.113.1:443"},
	{"multicast mDNS", "224.0.0.251:5353"},
	{"multicast SSDP", "239.255.255.250:1900"},
	{"240/4 reserved", "240.0.0.1:443"},
	{"limited broadcast", "255.255.255.255:9"},
	{"the line's own public IP", testObservedIP.String() + ":443"},
	{"SMTP", "11.22.33.44:25"},
	{"SMTPS", "11.22.33.44:465"},
	{"mail submission", "11.22.33.44:587"},
	{"SMTP over IPv6", "[2a00:1450:4001::1]:25"},
	{"IPv6 unspecified", "[::]:80"},
	{"IPv6 loopback", "[::1]:80"},
	{"IPv4-compatible IPv6", "[::10.0.0.1]:80"},
	{"NAT64 of a private IPv4", "[64:ff9b::10.0.0.1]:80"},
	{"NAT64 of IPv4 loopback", "[64:ff9b::127.0.0.1]:80"},
	{"local-use NAT64", "[64:ff9b:1::1]:80"},
	{"discard-only", "[100::1]:80"},
	{"Teredo and IETF assignments", "[2001::1]:80"},
	{"IPv6 documentation", "[2001:db8::1]:443"},
	{"IPv6 documentation (3fff::/20)", "[3fff::1]:443"},
	{"6to4 of a home router", "[2002:c0a8:b201::1]:80"},
	{"SRv6 SIDs", "[5f00::1]:443"},
	{"unique local first", "[fc00::1]:80"},
	{"unique local last", "[fdff:ffff:ffff:ffff:ffff:ffff:ffff:fffe]:80"},
	{"link-local", "[fe80::1]:80"},
	{"site-local", "[fec0::1]:80"},
	{"IPv6 multicast", "[ff02::fb]:5353"},
}

func TestEveryDeniedDestinationIsRefusedWithPolicy(t *testing.T) {
	// A loopback test origin is allowed, so the 127/8 cases also show that
	// the exception covers that one address and port only.
	origin := startTCPOrigin(t, func(*net.TCPConn) {})
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(origin))
	c := g.nextConn(t)

	for _, kind := range []relayprotocol.Kind{relayprotocol.KindTCP, relayprotocol.KindUDP} {
		for _, tc := range deniedDestinations {
			t.Run(kind.String()+"/"+tc.name, func(t *testing.T) {
				s, status := c.dial(t, kind, netip.MustParseAddrPort(tc.dst))
				if status != relayprotocol.StatusPolicy {
					t.Fatalf("status = %s, want POLICY", status)
				}
				if rest := readAll(t, s); len(rest) != 0 {
					t.Fatalf("agent sent %q after POLICY, want the end of the stream", rest)
				}
			})
		}
	}
}

func TestPolicyAllowsPublicDestinations(t *testing.T) {
	p := &policy{ownIP: testObservedIP, localNets: func() []netip.Prefix { return nil }}
	for _, dst := range []string{
		"11.22.33.44:443",
		"1.2.3.5:443", // next to the line's own IP
		"100.63.255.255:443",
		"100.128.0.0:443",
		"172.15.255.255:443",
		"172.32.0.0:443",
		"192.167.255.255:443",
		"192.169.0.0:443",
		"198.17.255.255:443",
		"198.20.0.0:443",
		"223.255.255.255:443",
		"11.22.33.44:24",
		"11.22.33.44:26",
		"11.22.33.44:586",
		"11.22.33.44:588",
		"[2a00:1450:4001::1]:443",
		"[64:ff9b::11.22.33.44]:443", // NAT64 of a public IPv4
		"[2002:b16:212c::1]:443",     // 6to4 of a public IPv4
	} {
		if err := p.check(netip.MustParseAddrPort(dst)); err != nil {
			t.Errorf("check(%s) = %v, want allowed", dst, err)
		}
	}
}

func TestPolicyRefusesTheNetworksOfTheMachinesInterfaces(t *testing.T) {
	// An IPv6 home network and a LAN with public IPv4 addresses are not in
	// any private range, but they are the member's home network.
	p := &policy{localNets: func() []netip.Prefix {
		return []netip.Prefix{
			netip.MustParsePrefix("2a02:8070:abcd:1200::/64"),
			netip.MustParsePrefix("93.184.0.0/24"),
		}
	}}
	for dst, wantRefused := range map[string]bool{
		"[2a02:8070:abcd:1200::42]:443": true,
		"93.184.0.17:443":               true,
		"[2a02:8070:abcd:1201::1]:443":  false,
		"93.184.1.17:443":               false,
	} {
		err := p.check(netip.MustParseAddrPort(dst))
		var polErr *policyError
		if refused := errors.As(err, &polErr); refused != wantRefused {
			t.Errorf("check(%s) = %v, want refused %v", dst, err, wantRefused)
		}
	}
}

func TestAnIPv4MappedTestOriginIsAllowedToo(t *testing.T) {
	origin := startTCPOrigin(t, func(*net.TCPConn) {})
	mapped := netip.AddrPortFrom(netip.AddrFrom16(origin.Addr().As16()), origin.Port())
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(mapped))
	g.nextConn(t).mustDial(t, relayprotocol.KindTCP, origin)
}

func TestANewSessionSeesTheMachinesCurrentNetworks(t *testing.T) {
	// A laptop that moves to another home network reconnects; its new
	// home /64 must be refused from the first flow on.
	current := "2a02:8070:aaaa:1::/64"
	restore := interfaceAddrs
	t.Cleanup(func() { interfaceAddrs = restore })
	interfaceAddrs = func() ([]net.Addr, error) {
		_, n, err := net.ParseCIDR(current)
		return []net.Addr{n}, err
	}
	p := newPolicy(testObservedIP, nil, nil)
	if err := p.check(netip.MustParseAddrPort("[2a02:8070:aaaa:1::9]:443")); err == nil {
		t.Fatal("the first network's address was allowed")
	}

	current = "2a02:8070:bbbb:1::/64"
	p = newPolicy(testObservedIP, nil, nil)
	if err := p.check(netip.MustParseAddrPort("[2a02:8070:bbbb:1::9]:443")); err == nil {
		t.Fatal("the new network's address was allowed right after the session started")
	}
}

func TestDialErrorsMapToFlowStatuses(t *testing.T) {
	opErr := func(err error) error { return &net.OpError{Op: "dial", Net: "tcp", Err: err} }
	for _, tc := range []struct {
		err  error
		want relayprotocol.Status
	}{
		{opErr(&policyError{rule: ruleSpecialRange}), relayprotocol.StatusPolicy},
		{opErr(os.NewSyscallError("connect", syscall.ECONNREFUSED)), relayprotocol.StatusRefused},
		{opErr(os.ErrDeadlineExceeded), relayprotocol.StatusTimeout},
		{context.DeadlineExceeded, relayprotocol.StatusTimeout},
		{opErr(os.NewSyscallError("connect", syscall.EHOSTUNREACH)), relayprotocol.StatusUnreachable},
		{opErr(os.NewSyscallError("connect", syscall.ENETUNREACH)), relayprotocol.StatusUnreachable},
	} {
		if got, _ := dialStatus(tc.err); got != tc.want {
			t.Errorf("dialStatus(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
}
