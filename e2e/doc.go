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
package e2e
