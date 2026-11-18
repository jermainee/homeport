package netwatch

import (
	"errors"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"testing"
)

var (
	probe4 = netip.MustParseAddrPort("192.0.2.1:9")
	probe6 = netip.MustParseAddrPort("[2001:db8::1]:9")
)

func TestCheckFindsVPNsAndTheHomeLine(t *testing.T) {
	errNoRoute := errors.New("no route")
	isTunnel := func(name string) bool { return strings.HasPrefix(name, "tun") }
	for _, tc := range []struct {
		name     string
		routes   map[netip.AddrPort]string // missing means no route
		defaults []string
		wantHome string
		wantVPN  bool
		wantErr  bool
	}{
		{"both on the LAN", map[netip.AddrPort]string{probe4: "en0", probe6: "en0"}, []string{"en0"}, "en0", false, false},
		{"IPv4 through a tunnel", map[netip.AddrPort]string{probe4: "tun4", probe6: "en0"}, []string{"tun4", "en0"}, "en0", true, false},
		{"only IPv6 through a tunnel", map[netip.AddrPort]string{probe4: "en0", probe6: "tun2"}, []string{"en0"}, "en0", true, false},
		{"no IPv6 route", map[netip.AddrPort]string{probe4: "en0"}, []string{"en0"}, "en0", false, false},
		{"no IPv6 route, IPv4 tunneled", map[netip.AddrPort]string{probe4: "tun0"}, []string{"en0"}, "en0", true, false},
		{"only a tunnel", map[netip.AddrPort]string{probe4: "tun0"}, []string{"tun0"}, "", true, false},
		{"offline", map[netip.AddrPort]string{}, nil, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := func(dst netip.AddrPort) (string, error) {
				if name, ok := tc.routes[dst]; ok {
					return name, nil
				}
				return "", errNoRoute
			}
			defaults := func() ([]string, error) { return tc.defaults, nil }
			home, vpn, err := check("", route, defaults, isTunnel)
			if home != tc.wantHome || vpn != tc.wantVPN || (err != nil) != tc.wantErr {
				t.Fatalf("check = %q, %v, %v; want %q, %v, error %v", home, vpn, err, tc.wantHome, tc.wantVPN, tc.wantErr)
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

func TestHomeIsWhereTheInternetRouteLeavesWhenVPNsAreSetAside(t *testing.T) {
	isTunnel := func(name string) bool { return strings.HasPrefix(name, "utun") }
	for _, tc := range []struct {
		name     string
		routed   []string // interfaces the probes leave through, in probe order
		defaults []string // interfaces that hold a default route, in table order
		prev     string
		want     string
	}{
		{"no VPN", []string{"en0"}, []string{"en0"}, "", "en0"},
		{"no VPN: the system's choice beats the last home", []string{"en10"}, []string{"en0", "en10"}, "en0", "en10"},
		{"VPN on: the LAN's default route is home", []string{"utun4"}, []string{"utun4", "en0"}, "", "en0"},
		{"VPN on: the last home stays home", []string{"utun4"}, []string{"utun4", "en0", "en10"}, "en10", "en10"},
		{"VPN on: the last home is gone", []string{"utun4"}, []string{"utun4", "en0"}, "en10", "en0"},
		{"only IPv6 through a VPN", []string{"en0", "utun4"}, []string{"en0"}, "", "en0"},
		{"only a VPN reaches the internet", []string{"utun4"}, []string{"utun4"}, "en0", ""},
		{"offline", nil, nil, "en0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := home(tc.routed, tc.defaults, tc.prev, isTunnel); got != tc.want {
				t.Fatalf("home = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckFindsThisMachinesHomeInterface(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("only macOS and Linux have a route check")
	}
	sys, err := outboundInterface(probe4)
	if err != nil {
		t.Skipf("no IPv4 route on this machine: %v", err)
	}
	if isTunnelInterface(sys) {
		t.Skipf("a VPN carries this machine's route (%s)", sys)
	}
	r, err := Check("")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if r.Home.Name != sys || r.Tunneled {
		t.Fatalf("Check = %+v, want home %s without a VPN", r, sys)
	}
	ifc, err := net.InterfaceByIndex(r.Home.Index)
	if err != nil || ifc.Name != sys {
		t.Fatalf("home index %d is not %s: %v", r.Home.Index, sys, err)
	}
	defaults, err := defaultRouteInterfaces()
	if err != nil || !slices.Contains(defaults, sys) {
		t.Fatalf("defaultRouteInterfaces = %v, %v; want it to hold %s", defaults, err, sys)
	}
}

func TestRoutedLeavesOutProbesWithoutARoute(t *testing.T) {
	errNoRoute := errors.New("no route")
	route := func(dst netip.AddrPort) (string, error) {
		if dst == probe4 {
			return "en0", nil
		}
		return "", errNoRoute
	}
	got, err := routed([]netip.AddrPort{probe4, probe6}, route)
	if err != nil || !slices.Equal(got, []string{"en0"}) {
		t.Fatalf("routed = %v, %v; want [en0]", got, err)
	}
	if _, err := routed([]netip.AddrPort{probe6}, route); err == nil {
		t.Fatal("routed found a route offline")
	}
}

func TestCheckReadsTheRouteTableOnlyBehindAVPN(t *testing.T) {
	route := func(netip.AddrPort) (string, error) { return "en0", nil }
	defaults := func() ([]string, error) {
		t.Error("check read the route table although no VPN carries the route")
		return nil, nil
	}
	isTunnel := func(name string) bool { return strings.HasPrefix(name, "utun") }
	if home, vpn, err := check("", route, defaults, isTunnel); home != "en0" || vpn || err != nil {
		t.Fatalf("check = %q, %v, %v; want en0 without a VPN", home, vpn, err)
	}
}
