package agent

import (
	"errors"
	"syscall"
)

// Winsock error numbers, which Go's net package returns on Windows.
const (
	wsaeconnaborted syscall.Errno = 10053
	wsaeconnreset   syscall.Errno = 10054
	wsaeconnrefused syscall.Errno = 10061
)

func isConnRefused(err error) bool { return errors.Is(err, wsaeconnrefused) }

func isConnReset(err error) bool {
	return errors.Is(err, wsaeconnreset) || errors.Is(err, wsaeconnaborted)
}
