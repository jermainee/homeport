//go:build e2e

package e2e

import (
	"bytes"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestAnHonestLinePassesTheCanary(t *testing.T) {
	s := newStack(t, withCanary(nil))

	waitUntil(t, "the agent served the canary's checks", func() bool {
		return s.agent.Stats().FlowsTotal >= 4 && s.resolverLookedInZone()
	})
	for _, e := range s.api.LineEvents() {
		if e.Type == "tampered" {
			t.Fatalf("an honest line was reported tampered: %+v", e)
		}
	}
	if !s.gw.Server().LineOnline(lineID) {
		t.Error("the line went offline")
	}
	// The member's traffic still works alongside the checks.
	s.get("/blob/1000")
}

func TestALineBehindAProxyThatChangesPagesIsPaused(t *testing.T) {
	s := newStack(t, withCanary(func(page netip.AddrPort) netip.AddrPort {
		return startRewritingProxy(t, page)
	}))

	waitUntil(t, "the gateway reported the line tampered", func() bool {
		for _, e := range s.api.LineEvents() {
			if e.Type == "tampered" && e.Check == "http" && e.LineID == lineID {
				return true
			}
		}
		return false
	})
	waitUntil(t, "the line went offline", func() bool { return !s.gw.Server().LineOnline(lineID) })
	// The agent keeps trying, and is refused.
	time.Sleep(time.Second)
	if s.gw.Server().LineOnline(lineID) {
		t.Error("the tampered line's agent got back in")
	}
}

// resolverLookedInZone reports whether the agent looked up a canary name.
func (s *stack) resolverLookedInZone() bool {
	s.resolver.mu.Lock()
	defer s.resolver.mu.Unlock()
	for _, l := range s.resolver.lookups {
		if bytes.HasSuffix([]byte(l), []byte("."+canaryZone)) {
			return true
		}
	}
	return false
}

// startRewritingProxy forwards each connection to target and injects a
// script into the HTML it sends back, as ad-injecting ISP proxies do.
func startRewritingProxy(t *testing.T, target netip.AddrPort) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				up, err := net.Dial("tcp", target.String())
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				go func() { _, _ = io.Copy(up, c) }()
				resp, _ := io.ReadAll(up)
				_, _ = c.Write(bytes.Replace(resp, []byte("</body>"), []byte("<script src=//ads.example/x.js></script></body>"), 1))
			}()
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String())
}
