package netwatch

import (
	"bufio"
	"cmp"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Check finds the home interface; prev is the last one it found. On Linux a
// VPN carries the internet route through a WireGuard or tun device, either
// with routes in the main table (OpenVPN's 0.0.0.0/1 and 128.0.0.0/1) or in
// a table of its own picked by a policy rule (wg-quick). Either way the
// LAN's default route stays in the main table.
func Check(prev string) (Route, error) {
	return lookup(check(prev, outboundInterface, defaultRouteInterfaces, isTunnelInterface))
}

// sysClassNet is where the kernel describes network devices. Tests replace
// it.
var sysClassNet = "/sys/class/net"

// arphrdNone is the device type of interfaces without a link layer:
// WireGuard and tun devices.
const arphrdNone = "65534"

// isTunnelInterface reports whether the device is a WireGuard or tun/tap
// device. A PPP device is not one: PPPoE is how many DSL lines connect.
func isTunnelInterface(name string) bool {
	// Each entry in sysClassNet is a symlink into /sys/devices, so an
	// os.Root, which refuses links that leave it, cannot read them. A
	// device name holds no path separator, so it cannot leave sysClassNet.
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return false
	}
	dir := filepath.Join(sysClassNet, name)
	if _, err := os.Stat(filepath.Join(dir, "tun_flags")); err == nil {
		return true
	}
	typ, err := os.ReadFile(filepath.Join(dir, "type")) //nolint:gosec // dir is one device's entry in sysClassNet, checked above.
	return err == nil && strings.TrimSpace(string(typ)) == arphrdNone
}

// defaultRouteInterfaces lists the interfaces that hold a default route in
// the main table, IPv4 first, each family by metric.
func defaultRouteInterfaces() ([]string, error) {
	var names []string
	var errs []error
	for _, src := range []struct {
		path  string
		parse func(io.Reader) []string
	}{
		{"/proc/net/route", parseProcNetRoute},
		{"/proc/net/ipv6_route", parseProcNetIPv6Route},
	} {
		f, err := os.Open(src.path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		names = append(names, src.parse(f)...)
		_ = f.Close()
	}
	if len(names) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return names, nil
}

// procRoute is a default route from /proc/net/route or /proc/net/ipv6_route.
type procRoute struct {
	iface  string
	metric uint64
}

// byMetric returns the routes' interfaces, lowest metric first.
func byMetric(routes []procRoute) []string {
	slices.SortStableFunc(routes, func(a, b procRoute) int { return cmp.Compare(a.metric, b.metric) })
	names := make([]string, 0, len(routes))
	for _, r := range routes {
		if r.iface != "lo" {
			names = append(names, r.iface)
		}
	}
	return names
}

// parseProcNetRoute returns the interfaces of the IPv4 default routes that
// are up, lowest metric first. Columns: Iface Destination Gateway Flags
// RefCnt Use Metric Mask, the addresses in hex.
func parseProcNetRoute(r io.Reader) []string {
	var routes []procRoute
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err1 := strconv.ParseUint(f[3], 16, 32)
		metric, err2 := strconv.ParseUint(f[6], 10, 32)
		if err1 != nil || err2 != nil || flags&unix.RTF_UP == 0 || flags&unix.RTF_REJECT != 0 {
			continue
		}
		routes = append(routes, procRoute{iface: f[0], metric: metric})
	}
	return byMetric(routes)
}

// parseProcNetIPv6Route returns the interfaces of the IPv6 default routes
// that are up and not unreachable, lowest metric first. Columns:
// destination, its prefix length, source, its prefix length, next hop,
// metric, reference count, use count, flags, interface, in hex.
func parseProcNetIPv6Route(r io.Reader) []string {
	var routes []procRoute
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[1] != "00" || strings.Trim(f[0], "0") != "" {
			continue
		}
		flags, err1 := strconv.ParseUint(f[8], 16, 32)
		metric, err2 := strconv.ParseUint(f[5], 16, 32)
		if err1 != nil || err2 != nil || flags&unix.RTF_UP == 0 || flags&unix.RTF_REJECT != 0 {
			continue
		}
		routes = append(routes, procRoute{iface: f[9], metric: metric})
	}
	return byMetric(routes)
}

// bindSocket ties the socket to the interface with SO_BINDTODEVICE, which an
// unprivileged process may do on a socket not bound to a device yet (Linux
// 5.7 and later). Route lookups for the socket then use only routes through
// that interface: a wg-quick tunnel's table holds none, so its policy rule
// falls through to the main table's default route.
func bindSocket(fd int, _ string, ifc Interface) error {
	return unix.BindToDevice(fd, ifc.Name)
}
