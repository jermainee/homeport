//go:build e2e

package main

// The e2e build tag lets the agent dial loopback test origins (see
// AllowTestOrigin in policy_testallow.go), which would open the member's own
// machine to relayed traffic. Release binaries must never carry it, so this
// command fails to compile with it.
var _ = theE2EBuildTagIsForTestsOnly
