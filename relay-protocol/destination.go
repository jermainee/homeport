package relayprotocol

import "net/netip"

// SpecialPurpose reports whether addr is in a private, local or other
// special-purpose range that is not globally reachable (the IANA
// special-purpose registries), in multicast, or is a NAT64 or 6to4 address
// that embeds such an IPv4 address. IPv4-mapped addresses are checked as
// IPv4. An invalid address, or one with an IPv6 zone, counts as special.
//
// Flows never go to these addresses: the gateway refuses them before any
// agent sees them, and the agent refuses them again with POLICY. The ranges
// include the tunnel networks (10.77.0.0/16 and fd77::/64), so members can
// never reach each other, and every network a home can number its devices
// in.
func SpecialPurpose(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.Zone() != "" {
		return true
	}
	for _, pfx := range specialPrefixes {
		if pfx.Contains(addr) {
			return true
		}
	}
	if v4, ok := embeddedIPv4(addr); ok {
		return SpecialPurpose(v4)
	}
	return false
}

// specialPrefixes are the ranges SpecialPurpose refuses directly.
var specialPrefixes = mustPrefixes(
	// IPv4
	"0.0.0.0/8",       // "this network"; connecting to 0.0.0.0 reaches the machine itself
	"10.0.0.0/8",      // private, including the IPv4 tunnel network 10.77.0.0/16
	"100.64.0.0/10",   // shared address space (carrier-grade NAT, Tailscale)
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, including cloud metadata services
	"172.16.0.0/12",   // private
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"192.88.99.0/24",  // deprecated 6to4 relay anycast
	"192.168.0.0/16",  // private
	"198.18.0.0/15",   // benchmarking, also used by fake-IP proxies
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, including the limited broadcast address
	// IPv6
	"::/96",          // unspecified, loopback and deprecated IPv4-compatible addresses
	"::ffff:0:0/96",  // IPv4-mapped; unmapped before the check, listed for safety
	"64:ff9b:1::/48", // local-use NAT64
	"100::/64",       // discard-only
	"2001::/23",      // IETF protocol assignments, including Teredo
	"2001:db8::/32",  // documentation
	"3fff::/20",      // documentation
	"5f00::/16",      // SRv6 segment identifiers
	"fc00::/7",       // unique local, including the IPv6 tunnel network fd77::/64
	"fe80::/10",      // link-local
	"fec0::/10",      // deprecated site-local
	"ff00::/8",       // multicast
)

// Prefixes that embed an IPv4 address, which gets the IPv4 rules.
var (
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour   = netip.MustParsePrefix("2002::/16")
)

// embeddedIPv4 returns the IPv4 address in a NAT64 (64:ff9b::/96) or 6to4
// (2002::/16) address.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	b := addr.As16()
	switch {
	case nat64Prefix.Contains(addr):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(addr):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}
