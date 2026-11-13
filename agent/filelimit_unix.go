//go:build unix

package agent

import (
	"log/slog"
	"syscall"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// wantOpenFiles leaves room for a socket per flow the gateway may open, plus
// the agent's own files and DNS lookups.
const wantOpenFiles = 2 * relayprotocol.AgentMaxIncomingStreams

// raiseFileLimit raises the soft limit on open files to wantOpenFiles if the
// hard limit allows it, and returns the soft limit in force. Go already
// raises it at startup on most systems; this covers the rest, such as a
// launcher that lowered it again.
func raiseFileLimit() (uint64, error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	if lim.Cur >= wantOpenFiles {
		return lim.Cur, nil
	}
	lim.Cur = min(wantOpenFiles, lim.Max)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	return lim.Cur, nil
}

// checkFileLimit raises the open file limit and warns if it stays too low
// for a socket per flow.
func checkFileLimit(log *slog.Logger) {
	n, err := raiseFileLimit()
	if err != nil {
		log.Warn("could not raise the open file limit", "error", err)
		return
	}
	if n <= relayprotocol.AgentMaxIncomingStreams {
		log.Warn("open file limit is too low for a socket per flow; flows will fail once it runs out",
			"limit", n, "flows", relayprotocol.AgentMaxIncomingStreams)
	}
}
