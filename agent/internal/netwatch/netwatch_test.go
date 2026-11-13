package netwatch

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

var (
	probe4 = netip.MustParseAddrPort("192.0.2.1:9")
	probe6 = netip.MustParseAddrPort("[2001:db8::1]:9")
)

func TestTunneledWhenAnyProbeLeavesThroughATunnel(t *testing.T) {
	errNoRoute := errors.New("no route")
	isTunnel := func(name string) bool { return strings.HasPrefix(name, "tun") }
	for _, tc := range []struct {
		name    string
		routes  map[netip.AddrPort]string // missing means no route
		want    bool
		wantErr bool
	}{
		{"both on the LAN", map[netip.AddrPort]string{probe4: "en0", probe6: "en0"}, false, false},
		{"IPv4 through a tunnel", map[netip.AddrPort]string{probe4: "tun4", probe6: "en0"}, true, false},
		{"only IPv6 through a tunnel", map[netip.AddrPort]string{probe4: "en0", probe6: "tun2"}, true, false},
		{"no IPv6 route", map[netip.AddrPort]string{probe4: "en0"}, false, false},
		{"no IPv6 route, IPv4 tunneled", map[netip.AddrPort]string{probe4: "tun0"}, true, false},
		{"offline", map[netip.AddrPort]string{}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := func(dst netip.AddrPort) (string, error) {
				if name, ok := tc.routes[dst]; ok {
					return name, nil
				}
				return "", errNoRoute
			}
			got, err := tunneled([]netip.AddrPort{probe4, probe6}, route, isTunnel)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("tunneled = %v, %v; want %v, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestOutboundInterfaceNamesARealInterface(t *testing.T) {
	name, err := outboundInterface(probe4)
	if err != nil {
		t.Skipf("no IPv4 route on this machine: %v", err)
	}
	ifc, err := net.InterfaceByName(name)
	if err != nil || ifc.Name != name {
		t.Fatalf("outboundInterface = %q, which is not an interface: %v", name, err)
	}
}
