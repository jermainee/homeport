//go:build e2e

package agent

import "net/netip"

// AllowTestOrigin lets the agent configured by cfg dial origin, a loopback
// test server that the policy would otherwise refuse. New rejects an origin
// that is not a loopback address.
//
// Only builds with the e2e tag have this function, for the end-to-end tests
// in e2e/. cmd/homeport-agent does not compile with that tag. A program that
// embeds this package, such as the desktop app, must refuse the tag in its
// own release build as well.
func AllowTestOrigin(cfg *Config, origin netip.AddrPort) {
	cfg.testOrigins = append(cfg.testOrigins, origin)
}
