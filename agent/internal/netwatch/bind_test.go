//go:build darwin || linux

package netwatch

import (
	"net"
	"net/netip"
	"syscall"
	"testing"
)

func loopbackInterface(t *testing.T) Interface {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 {
			return Interface{Index: ifc.Index, Name: ifc.Name}
		}
	}
	t.Skip("no loopback interface")
	return Interface{}
}

// boundDialer binds every socket it opens to ifc.
func boundDialer(ifc Interface) *net.Dialer {
	return &net.Dialer{Control: func(network, address string, c syscall.RawConn) error {
		return Bind(c, network, netip.MustParseAddrPort(address).Addr(), ifc)
	}}
}

func TestBindTiesTheSocketToTheInterface(t *testing.T) {
	r, err := Check("")
	if err != nil || r.Home.Name == "" {
		t.Skipf("no home interface on this machine: %+v, %v", r, err)
	}
	c, err := boundDialer(r.Home).Dial("udp4", probe4.String())
	if err != nil {
		t.Fatalf("socket bound to %s has no route to %v: %v", r.Home.Name, probe4, err)
	}
	defer func() { _ = c.Close() }()
	if got := boundTo(t, c.(*net.UDPConn), "udp4"); got != r.Home.Name {
		t.Fatalf("socket bound to %q, want %q", got, r.Home.Name)
	}
}

func TestABoundSocketStillReachesLoopback(t *testing.T) {
	home, err := Check("")
	if err != nil || home.Home.Name == "" {
		t.Skipf("no home interface on this machine: %+v, %v", home, err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	c, err := boundDialer(home.Home).Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("socket bound to %s cannot reach loopback: %v", home.Home.Name, err)
	}
	defer func() { _ = c.Close() }()
	if got := boundTo(t, c.(*net.TCPConn), "tcp4"); got != "" {
		t.Fatalf("socket to loopback bound to %q", got)
	}
}

func TestBindingToNoInterfaceLeavesTheSocketAlone(t *testing.T) {
	unbound, err := net.Dial("udp4", probe4.String())
	if err != nil {
		t.Skipf("no IPv4 route on this machine: %v", err)
	}
	want := unbound.LocalAddr().(*net.UDPAddr).AddrPort().Addr()
	_ = unbound.Close()
	c, err := boundDialer(Interface{}).Dial("udp4", probe4.String())
	if err != nil {
		t.Fatalf("Bind with no interface broke the socket: %v", err)
	}
	defer func() { _ = c.Close() }()
	if got := c.LocalAddr().(*net.UDPAddr).AddrPort().Addr(); got != want {
		t.Fatalf("socket sends from %v, want %v", got, want)
	}
}
