package netwatch

import (
	"net"
	"strings"
	"syscall"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// Check finds the home interface; prev is the last one it found. On macOS a
// VPN carries the internet route through a utun interface (WireGuard,
// OpenVPN and Network Extension VPNs) or an ipsec interface (macOS's own
// IKEv2 VPN), while the LAN's default route stays in the table as a route
// scoped to its interface.
func Check(prev string) (Route, error) {
	return lookup(check(prev, outboundInterface, defaultRouteInterfaces, isTunnelInterface))
}

func isTunnelInterface(name string) bool {
	return strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "ipsec")
}

// defaultRouteInterfaces lists the interfaces that hold a default route, in
// table order: up, not loopback, scoped routes included.
func defaultRouteInterfaces() ([]string, error) {
	rib, err := route.FetchRIB(syscall.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, err
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok || rm.Flags&unix.RTF_UP == 0 || rm.Flags&(unix.RTF_HOST|unix.RTF_REJECT|unix.RTF_BLACKHOLE) != 0 {
			continue
		}
		if len(rm.Addrs) <= unix.RTAX_NETMASK || !isZero(rm.Addrs[unix.RTAX_DST]) {
			continue
		}
		if mask := rm.Addrs[unix.RTAX_NETMASK]; mask != nil && !isZero(mask) {
			continue
		}
		ifc, err := net.InterfaceByIndex(rm.Index)
		if err != nil || ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		names = append(names, ifc.Name)
	}
	return names, nil
}

// isZero reports whether a is the IPv4 or IPv6 unspecified address.
func isZero(a route.Addr) bool {
	switch a := a.(type) {
	case *route.Inet4Addr:
		return a.IP == [4]byte{}
	case *route.Inet6Addr:
		return a.IP == [16]byte{}
	}
	return false
}

// bindSocket ties the socket to the interface: the kernel then routes its
// packets through that interface's routes only, scoped routes included,
// whatever carries the default route.
func bindSocket(fd int, network string, ifc Interface) error {
	if strings.HasSuffix(network, "6") { // "tcp6" or "udp6"
		return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, ifc.Index)
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_BOUND_IF, ifc.Index)
}
