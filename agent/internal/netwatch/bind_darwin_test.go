package netwatch

import (
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// boundTo returns the interface the socket is bound to, or "".
func boundTo(t *testing.T, c syscall.Conn, network string) string {
	t.Helper()
	raw, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var idx int
	var optErr error
	_ = raw.Control(func(fd uintptr) {
		if network[len(network)-1] == '6' {
			idx, optErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF)
		} else {
			idx, optErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF)
		}
	})
	if optErr != nil {
		t.Fatal(optErr)
	}
	if idx == 0 {
		return ""
	}
	ifc, err := net.InterfaceByIndex(idx)
	if err != nil {
		t.Fatal(err)
	}
	return ifc.Name
}

func TestABoundSocketLeavesOnlyThroughItsInterface(t *testing.T) {
	unbound, err := net.Dial("udp4", probe4.String())
	if err != nil {
		t.Skipf("no IPv4 route on this machine: %v", err)
	}
	viaDefault := unbound.LocalAddr().(*net.UDPAddr).AddrPort().Addr()
	_ = unbound.Close()

	// Bound to loopback, the socket cannot use the default route: macOS
	// finds no route through lo0.
	c, err := boundDialer(loopbackInterface(t)).Dial("udp4", probe4.String())
	if err == nil {
		_ = c.Close()
		t.Fatalf("a socket bound to loopback reached %v from %v; the default route's address is %v", probe4, c.LocalAddr(), viaDefault)
	}
}
