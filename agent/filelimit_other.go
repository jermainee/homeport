//go:build !unix

package agent

import "log/slog"

// checkFileLimit is a no-op where open sockets have no per-process limit
// to raise, such as on Windows.
func checkFileLimit(*slog.Logger) {}
