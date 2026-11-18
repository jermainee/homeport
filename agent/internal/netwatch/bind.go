package netwatch

import (
	"net/netip"
	"syscall"
)

// Bind ties the socket c, for network ("tcp4", "udp6" and so on) and
// destination dst, to the interface ifc, so its packets leave through ifc
// whatever carries the machine's default route. It leaves the socket alone
// when ifc is zero, and when dst is a loopback address, which never leaves
// the machine. Call it from a net.Dialer's or net.ListenConfig's Control
// hook, before the socket connects.
func Bind(c syscall.RawConn, network string, dst netip.Addr, ifc Interface) error {
	if ifc.Index == 0 || dst.Unmap().IsLoopback() {
		return nil
	}
	var bindErr error
	if err := c.Control(func(fd uintptr) { bindErr = bindSocket(int(fd), network, ifc) }); err != nil {
		return err
	}
	return bindErr
}
