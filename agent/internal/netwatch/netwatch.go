// Package netwatch finds the machine's home line: the interface that reaches
// the internet when VPNs are set aside. The agent binds its sockets to it, so
// shared traffic leaves through the member's own line even while a VPN, such
// as a Homeport Use tunnel, carries the machine's default route.
package netwatch

import (
	"errors"
	"net"
	"net/netip"
)

// Interface names a network interface.
type Interface struct {
	Index int
	Name  string
}

// Route is how the machine reaches the internet.
type Route struct {
	// Home is the interface that reaches the internet when VPNs are set
	// aside: the home line. It is zero when only tunnels reach the
	// internet, and on systems without a route check.
	Home Interface
	// Tunneled reports that a VPN carries the internet route.
	Tunneled bool
}

// probes are documentation addresses that stand for "the internet".
// Connecting a UDP socket to one only looks up its route; nothing is sent.
var probes = []netip.AddrPort{
	netip.MustParseAddrPort("192.0.2.1:9"),
	netip.MustParseAddrPort("[2001:db8::1]:9"),
}

// lookup resolves the home interface's name to its index.
func lookup(name string, tunneled bool, err error) (Route, error) {
	r := Route{Tunneled: tunneled}
	if err != nil || name == "" {
		return r, err
	}
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return Route{}, err
	}
	r.Home = Interface{Index: ifc.Index, Name: ifc.Name}
	return r, nil
}

// check is Check with the system's lookups passed in. It returns the home
// interface's name and whether a VPN carries the internet route.
func check(prev string, route func(netip.AddrPort) (string, error), defaults func() ([]string, error), isTunnel func(string) bool) (string, bool, error) {
	names, routeErr := routed(probes, route)
	defs, defErr := defaults()
	if len(names) == 0 && len(defs) == 0 {
		return "", false, errors.Join(routeErr, defErr)
	}
	tunneled := false
	for _, name := range names {
		if isTunnel(name) {
			tunneled = true
		}
	}
	return home(names, defs, prev, isTunnel), tunneled, nil
}

// routed returns the interfaces the probes leave through, in probe order. A
// probe without a route is skipped, so a machine without IPv6 is judged by
// IPv4 alone; it is an error only if no probe has a route.
func routed(probes []netip.AddrPort, route func(netip.AddrPort) (string, error)) ([]string, error) {
	var names []string
	var errs []error
	for _, p := range probes {
		name, err := route(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.Join(errs...)
	}
	return names, nil
}

// home picks the interface that reaches the internet when VPNs are set
// aside: the home line. routed are the interfaces the probes leave through,
// in probe order; defaults are the interfaces that hold a default route, in
// table order; prev is the last home.
//
// Without a VPN the system's own choice is home. While a VPN carries the
// internet route, home is a default route that is not a tunnel's: the last
// home if it still has one, so a VPN coming up never moves the line, or else
// the first. It is empty when only tunnels reach the internet.
func home(routed, defaults []string, prev string, isTunnel func(string) bool) string {
	for _, name := range routed {
		if !isTunnel(name) {
			return name
		}
	}
	first := ""
	for _, name := range defaults {
		if isTunnel(name) {
			continue
		}
		if name == prev {
			return name
		}
		if first == "" {
			first = name
		}
	}
	return first
}

// outboundInterface returns the name of the interface the system would send
// a packet to dst through: the interface that holds the source address the
// kernel picks for it.
func outboundInterface(dst netip.AddrPort) (string, error) {
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(dst))
	if err != nil {
		return "", err
	}
	src := c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	_ = c.Close()

	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip, ok := netip.AddrFromSlice(ipnet.IP); ok && ip.Unmap() == src.WithZone("") {
				return ifc.Name, nil
			}
		}
	}
	return "", errors.New("netwatch: no interface holds the route's source address")
}
