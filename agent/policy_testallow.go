//go:build e2e

package agent

import (
	"maps"
	"net/netip"
	"slices"
)

// AllowTestOrigin lets the agent configured by cfg dial origin, a loopback
// test server that the policy would otherwise refuse. New rejects an origin
// that is not a loopback address.
//
// Only builds with the e2e tag have this function, for the end-to-end tests
// in e2e/. cmd/homeport-agent does not compile with that tag. A program that
// embeds this package, such as the desktop app, must refuse the tag in its
// own release build as well.
func AllowTestOrigin(cfg *Config, origin netip.AddrPort) {
	// Clipped, so an origin added to a copy of cfg never overwrites one of cfg's.
	cfg.testOrigins = append(slices.Clip(cfg.testOrigins), origin)
}

// RouteTestOrigin makes the agent configured by cfg connect flows to dst to
// origin instead, a loopback test server. A real gateway drops packets
// addressed to loopback, so the end-to-end tests send their flows to a
// public-looking dst, and only the agent's last step, the socket, reaches
// the test server. DNS answers may name dst's address. New rejects an
// origin that is not a loopback address.
//
// A route covers TCP and UDP flows to dst alike, so give TCP and UDP
// origins different destinations. Like a test origin, a routed dst skips
// the policy; only other ports of its address get the policy as usual, so
// do not route a mail port to test that mail is refused.
//
// Like AllowTestOrigin, it exists only in builds with the e2e tag.
func RouteTestOrigin(cfg *Config, dst, origin netip.AddrPort) {
	// A copy, so routes added to a copy of cfg stay out of cfg.
	routes := maps.Clone(cfg.testRoutes)
	if routes == nil {
		routes = make(map[netip.AddrPort]netip.AddrPort)
	}
	routes[dst] = origin
	cfg.testRoutes = routes
	AllowTestOrigin(cfg, origin)
}
