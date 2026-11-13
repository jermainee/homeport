//go:build unix

package agent

import (
	"syscall"
	"testing"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

func TestTheAgentMayHoldASocketForEveryFlow(t *testing.T) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	if lim.Max < wantOpenFiles {
		t.Skipf("hard limit %d is below %d; only an administrator can raise it", lim.Max, wantOpenFiles)
	}
	// Start from the macOS default soft limit.
	low := lim
	low.Cur = 256
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim) })

	got, err := raiseFileLimit()
	if err != nil {
		t.Fatal(err)
	}
	if got <= relayprotocol.AgentMaxIncomingStreams {
		t.Fatalf("open file limit %d, want more than the %d flows a gateway may open", got, relayprotocol.AgentMaxIncomingStreams)
	}
}
