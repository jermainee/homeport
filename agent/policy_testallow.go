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

// RouteTestOrigin makes the agent configured by cfg connect flows to dst to
// origin instead, a loopback test server. A real gateway drops packets
// addressed to loopback, so the end-to-end tests send their flows to a
// public-looking dst, and only the agent's last step, the socket, reaches
// the test server. DNS answers may name dst's address. Other ports of that
// address get the policy as usual. New rejects an origin that is not a
// loopback address.
//
// Like AllowTestOrigin, it exists only in builds with the e2e tag.
func RouteTestOrigin(cfg *Config, dst, origin netip.AddrPort) {
	if cfg.testRoutes == nil {
		cfg.testRoutes = make(map[netip.AddrPort]netip.AddrPort)
	}
	cfg.testRoutes[dst] = origin
	cfg.testOrigins = append(cfg.testOrigins, origin)
}
