//go:build e2e

package main

// The e2e build tag lets the embedded sharing agent dial loopback test
// origins (agent.AllowTestOrigin and agent.RouteTestOrigin), which would open
// the member's own computer to relayed traffic. The app must never carry it,
// so it fails to compile with the tag.
var _ = theE2EBuildTagIsForTestsOnly
