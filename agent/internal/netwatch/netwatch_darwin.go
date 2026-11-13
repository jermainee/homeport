package netwatch

import (
	"net/netip"
	"strings"
)

// probes are documentation addresses that stand for "the internet".
// Connecting a UDP socket to one only looks up its route; nothing is sent.
var probes = []netip.AddrPort{
	netip.MustParseAddrPort("192.0.2.1:9"),
	netip.MustParseAddrPort("[2001:db8::1]:9"),
}

// Tunneled reports whether another VPN carries the default route: whether
// traffic to the internet leaves through a utun interface (WireGuard,
// OpenVPN and Network Extension VPNs) or an ipsec interface (macOS's own
// IKEv2 VPN).
func Tunneled() (bool, error) {
	return tunneled(probes, outboundInterface, isTunnelInterface)
}

func isTunnelInterface(name string) bool {
	return strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "ipsec")
}
