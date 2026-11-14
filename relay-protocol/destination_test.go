package relayprotocol_test

import (
	"net/netip"
	"testing"

	rp "github.com/jermainee/homeport/relay-protocol"
)

// specialPurpose holds an address for every range neither side may open a
// flow to, with the edges of each range.
var specialPurpose = []struct{ name, addr string }{
	{"this network", "0.0.0.0"},
	{"10/8 first", "10.0.0.0"},
	{"10/8 last", "10.255.255.255"},
	{"the gateway's tunnel address", "10.77.0.1"},
	{"another member's tunnel address", "10.77.0.3"},
	{"100.64/10 first", "100.64.0.0"},
	{"100.64/10 last", "100.127.255.255"},
	{"127/8 first", "127.0.0.1"},
	{"127/8 last", "127.255.255.255"},
	{"169.254/16, cloud metadata", "169.254.169.254"},
	{"172.16/12 first", "172.16.0.0"},
	{"172.16/12 last", "172.31.255.255"},
	{"192.0.0/24 protocol assignments", "192.0.0.170"},
	{"192.0.2/24 documentation", "192.0.2.1"},
	{"192.88.99/24 6to4 relay anycast", "192.88.99.1"},
	{"192.168/16 first", "192.168.0.0"},
	{"192.168/16 home router", "192.168.178.1"},
	{"192.168/16 last", "192.168.255.255"},
	{"198.18/15 first", "198.18.0.0"},
	{"198.18/15 last", "198.19.255.255"},
	{"198.51.100/24 documentation", "198.51.100.1"},
	{"203.0.113/24 documentation", "203.0.113.1"},
	{"multicast first", "224.0.0.0"},
	{"multicast SSDP", "239.255.255.250"},
	{"240/4 reserved", "240.0.0.1"},
	{"limited broadcast", "255.255.255.255"},
	{"IPv6 unspecified", "::"},
	{"IPv6 loopback", "::1"},
	{"IPv4-compatible IPv6", "::10.0.0.1"},
	{"IPv4-mapped private IPv4", "::ffff:192.168.178.1"},
	{"IPv4-mapped tunnel address", "::ffff:10.77.0.3"},
	{"NAT64 of a private IPv4", "64:ff9b::10.0.0.1"},
	{"NAT64 of a tunnel address", "64:ff9b::10.77.0.3"},
	{"NAT64 of IPv4 loopback", "64:ff9b::127.0.0.1"},
	{"local-use NAT64", "64:ff9b:1::1"},
	{"discard-only", "100::1"},
	{"Teredo and IETF assignments", "2001::1"},
	{"IPv6 documentation", "2001:db8::1"},
	{"IPv6 documentation (3fff::/20)", "3fff::1"},
	{"6to4 of a home router", "2002:c0a8:b201::1"},
	{"SRv6 SIDs", "5f00::1"},
	{"unique local first", "fc00::"},
	{"the gateway's IPv6 tunnel address", "fd77::1"},
	{"another member's IPv6 tunnel address", "fd77::3"},
	{"unique local last", "fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"},
	{"link-local", "fe80::1"},
	{"link-local with a zone", "fe80::1%en0"},
	{"site-local", "fec0::1"},
	{"IPv6 multicast", "ff02::fb"},
}

// globallyReachable are addresses outside every special-purpose range,
// including the edges right next to them.
var globallyReachable = []string{
	"1.1.1.1",
	"9.255.255.255",
	"11.0.0.0",
	"11.22.33.44",
	"100.63.255.255",
	"100.128.0.0",
	"126.255.255.255",
	"128.0.0.0",
	"172.15.255.255",
	"172.32.0.0",
	"192.167.255.255",
	"192.169.0.0",
	"198.17.255.255",
	"198.20.0.0",
	"223.255.255.255",
	"2a00:1450:4001::1",
	"2606:4700:4700::1111",
	"64:ff9b::b16:212c",     // NAT64 of 11.22.33.44
	"2002:b16:212c::1",      // 6to4 of 11.22.33.44
	"2001:200::1",           // just past 2001::/23
	"2001:db7:ffff::1",      // just below 2001:db8::/32
	"2001:db9::1",           // just past 2001:db8::/32
	"fbff:ffff:ffff::1",     // just below fc00::/7
	"3ffe:ffff:ffff::1",     // just below 3fff::/20
	"3fff:1000::1",          // just past 3fff::/20
	"5eff:ffff:ffff::1",     // just below 5f00::/16
	"::ffff:11.22.33.44",    // IPv4-mapped, checked as IPv4
	"64:ff9b:0:1::b16:212c", // between 64:ff9b::/96 and 64:ff9b:1::/48
}

func TestSpecialPurposeAddressesAreRecognized(t *testing.T) {
	for _, tc := range specialPurpose {
		t.Run(tc.name, func(t *testing.T) {
			if !rp.SpecialPurpose(netip.MustParseAddr(tc.addr)) {
				t.Errorf("SpecialPurpose(%s) = false, want true", tc.addr)
			}
		})
	}
}

func TestGloballyReachableAddressesAreNotSpecialPurpose(t *testing.T) {
	for _, a := range globallyReachable {
		t.Run(a, func(t *testing.T) {
			if rp.SpecialPurpose(netip.MustParseAddr(a)) {
				t.Errorf("SpecialPurpose(%s) = true, want false", a)
			}
		})
	}
}

func TestTheZeroAddressIsSpecialPurpose(t *testing.T) {
	if !rp.SpecialPurpose(netip.Addr{}) {
		t.Error("SpecialPurpose(netip.Addr{}) = false, want true")
	}
}
