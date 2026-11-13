package agent

import (
	"runtime"
	"testing"
	"time"
)

func TestAgentSaysHelloAndServesTheAcknowledgedLine(t *testing.T) {
	g := newFakeGateway(t)
	a := startAgent(t, g, nil)

	c := g.nextConn(t)
	if c.hello.OS != runtime.GOOS || c.hello.AgentVersion == "" {
		t.Fatalf("hello = %+v, want os %q and a version", c.hello, runtime.GOOS)
	}
	waitState(t, a.Agent, StateConnected)
	if got := a.Stats().LineID; got != testLineID {
		t.Fatalf("Stats().LineID = %q, want %q", got, testLineID)
	}
}

// waitState waits until the agent reports state.
func waitState(t *testing.T, a *Agent, want State) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if a.Stats().State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("agent state %s, want %s", a.Stats().State, want)
}
