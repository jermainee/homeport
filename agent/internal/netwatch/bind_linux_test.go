package netwatch

import (
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// boundTo returns the device the socket is bound to, or "".
func boundTo(t *testing.T, c syscall.Conn, _ string) string {
	t.Helper()
	raw, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var name string
	var optErr error
	_ = raw.Control(func(fd uintptr) {
		name, optErr = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
	})
	if optErr != nil {
		t.Fatal(optErr)
	}
	return name
}
