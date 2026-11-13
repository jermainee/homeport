// Package netwatch tells whether another VPN carries the machine's default
// route. While one does, the agent pauses sharing, so shared traffic never
// leaves through someone else's tunnel.
package netwatch

import (
	"errors"
	"net"
	"net/netip"
)

// tunneled reports whether the route to any probe leaves through an
// interface that isTunnel accepts. A probe without a route is skipped, so a
// machine without IPv6 is judged by IPv4 alone; it is an error only if no
// probe has a route.
func tunneled(probes []netip.AddrPort, route func(netip.AddrPort) (string, error), isTunnel func(string) bool) (bool, error) {
	var errs []error
	for _, p := range probes {
		name, err := route(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if isTunnel(name) {
			return true, nil
		}
	}
	if len(errs) == len(probes) {
		return false, errors.Join(errs...)
	}
	return false, nil
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
