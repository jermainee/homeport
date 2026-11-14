package gateway

import (
	"net/netip"
	"testing"
)

func TestDestinationAllowed(t *testing.T) {
	dns := netip.MustParseAddr("10.77.0.1")
	for dst, want := range map[string]bool{
		"10.77.0.1:53":             true, // the virtual resolver
		"11.22.33.44:443":          true,
		"[2a00:1450:4001::1]:443":  true,
		"10.77.0.1:80":             false, // the gateway on another port
		"10.77.0.1:5353":           false,
		"[fd77::1]:53":             false, // the virtual resolver is IPv4 only
		"10.77.0.3:53":             false, // another member
		"[fd77::3]:443":            false,
		"[::ffff:10.77.0.1]:53":    false, // IPv4-mapped never appears in a real packet
		"[::ffff:10.77.0.3]:443":   false,
		"[::ffff:11.22.33.44]:443": false,
		"[64:ff9b::a4d:3]:443":     false, // NAT64 of another member
		"[64:ff9b::b16:212c]:443":  true,  // NAT64 of a public address
		"192.168.178.1:80":         false,
		"[fe80::1%wg0]:80":         false,
	} {
		if got := destinationAllowed(dns, netip.MustParseAddrPort(dst)); got != want {
			t.Errorf("destinationAllowed(%s) = %v, want %v", dst, got, want)
		}
	}
}
