// Package e2e holds Homeport's cross-folder end-to-end tests. They run the
// real gateway (through gateway/service, as production does) from a fake
// control API, the real sharing agent, and a userspace WireGuard client, all
// in one process, and check that traffic arrives intact, that every byte is
// metered and reported exactly once, and that the logs never name a
// destination.
//
// The tests need the e2e build tag, which gives the agent its test hooks:
//
//	GOWORK=off go test -race -count=1 -tags e2e ./...
//
// TestFullStackSmoke, behind the smoke tag as well, runs the real control
// API (wrangler dev on a fresh local D1) and the gateway binary instead of
// the fake API and the in-process gateway, and checks the money path from
// tunnel bytes to ledger entries. It needs npm ci in api/ first:
//
//	GOWORK=off go test -count=1 -tags e2e,smoke -run TestFullStackSmoke .
package e2e
