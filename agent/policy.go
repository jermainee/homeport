package agent

import (
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"
)

// policy decides which destinations the agent may dial, so that a member
// who uses the line can never reach into the sharing member's home network
// ("Seller home network protection" in docs/concept.md) or send mail.
//
// It runs in the dialer's Control hook, on the address the socket is about to
// connect to, so it also covers addresses that came out of DNS.
type policy struct {
	// ownIP is the line's public IP as the gateway sees it.
	ownIP netip.Addr
	// localNets returns the networks of the machine's own interfaces.
	localNets func() []netip.Prefix
	// testOrigins are loopback test servers allowed in tests only.
	testOrigins []netip.AddrPort
}

// newPolicy returns the policy for one connection. It drops the cached
// interface networks, so a machine that moved to another network refuses the
// new home network from the connection's first flow on.
func newPolicy(ownIP netip.Addr, testOrigins []netip.AddrPort) *policy {
	ifNets.mu.Lock()
	ifNets.fetched = time.Time{} // refetch, but keep the last answer if that fails
	ifNets.mu.Unlock()
	return &policy{ownIP: ownIP.Unmap(), localNets: interfaceNets, testOrigins: testOrigins}
}

// policyError is the policy's refusal of a destination. It names the rule,
// never the destination, so it is safe to log.
type policyError struct{ rule string }

func (e *policyError) Error() string { return "destination refused by policy: " + e.rule }

// Policy rules, as they appear in logs.
const (
	ruleBlockedPort  = "blocked_port"
	ruleSpecialRange = "special_range"
	ruleOwnIP        = "own_ip"
	ruleLocalNetwork = "local_network"
	ruleUnparsable   = "unparsable_address"
)

// blockedPorts are mail ports: SMTP, SMTPS and submission.
var blockedPorts = map[uint16]bool{25: true, 465: true, 587: true}

// deniedPrefixes are private, local and special-purpose ranges that are not
// globally reachable (the IANA special-purpose registries), plus multicast.
var deniedPrefixes = mustPrefixes(
	// IPv4
	"0.0.0.0/8",       // "this network"; connecting to 0.0.0.0 reaches the machine itself
	"10.0.0.0/8",      // private
	"100.64.0.0/10",   // shared address space (carrier-grade NAT, Tailscale)
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, including cloud metadata services
	"172.16.0.0/12",   // private
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"192.88.99.0/24",  // deprecated 6to4 relay anycast
	"192.168.0.0/16",  // private
	"198.18.0.0/15",   // benchmarking, also used by fake-IP proxies
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, including the limited broadcast address
	// IPv6
	"::/96",          // unspecified, loopback and deprecated IPv4-compatible addresses
	"::ffff:0:0/96",  // IPv4-mapped; unmapped before the check, listed for safety
	"64:ff9b:1::/48", // local-use NAT64
	"100::/64",       // discard-only
	"2001::/23",      // IETF protocol assignments, including Teredo
	"2001:db8::/32",  // documentation
	"3fff::/20",      // documentation
	"5f00::/16",      // SRv6 segment identifiers
	"fc00::/7",       // unique local
	"fe80::/10",      // link-local
	"fec0::/10",      // deprecated site-local
	"ff00::/8",       // multicast
)

// Prefixes that embed an IPv4 address, which gets the IPv4 rules.
var (
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour   = netip.MustParsePrefix("2002::/16")
)

// check returns a *policyError if the policy refuses dst.
func (p *policy) check(dst netip.AddrPort) error {
	addr := dst.Addr().Unmap()
	dst = netip.AddrPortFrom(addr, dst.Port())
	for _, o := range p.testOrigins {
		if dst == o {
			return nil
		}
	}
	if blockedPorts[dst.Port()] {
		return &policyError{rule: ruleBlockedPort}
	}
	return p.checkAddr(addr)
}

// checkAddr applies the address rules of check, without the port rule.
func (p *policy) checkAddr(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.Zone() != "" {
		return &policyError{rule: ruleUnparsable}
	}
	if p.ownIP.IsValid() && addr == p.ownIP {
		return &policyError{rule: ruleOwnIP}
	}
	if deniedAddr(addr) {
		return &policyError{rule: ruleSpecialRange}
	}
	if p.localNets != nil {
		for _, n := range p.localNets() {
			if n.Contains(addr) {
				return &policyError{rule: ruleLocalNetwork}
			}
		}
	}
	return nil
}

// deniedAddr reports whether addr is in a denied range, directly or as the
// IPv4 address inside a NAT64 or 6to4 address.
func deniedAddr(addr netip.Addr) bool {
	for _, pfx := range deniedPrefixes {
		if pfx.Contains(addr) {
			return true
		}
	}
	if v4, ok := embeddedIPv4(addr); ok {
		return deniedAddr(v4)
	}
	return false
}

// embeddedIPv4 returns the IPv4 address in a NAT64 (64:ff9b::/96) or 6to4
// (2002::/16) address.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	b := addr.As16()
	switch {
	case nat64Prefix.Contains(addr):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(addr):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

// allowsAnswer reports whether a DNS answer may hold addr: whether the
// policy would let a flow reach it on some port.
func (p *policy) allowsAnswer(addr netip.Addr) bool {
	for _, o := range p.testOrigins {
		if addr.Unmap() == o.Addr() {
			return true
		}
	}
	return p.checkAddr(addr) == nil
}

// dialer returns a dialer that applies the policy in its Control hook, which
// runs on the resolved address right before connect.
func (p *policy) dialer() *net.Dialer {
	return &net.Dialer{Timeout: dialTimeout, Control: p.control}
}

func (p *policy) control(_, address string, _ syscall.RawConn) error {
	dst, err := netip.ParseAddrPort(address)
	if err != nil {
		return &policyError{rule: ruleUnparsable}
	}
	return p.check(dst)
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// interfaceNetsTTL is how long interfaceNets reuses its answer. Addresses
// change rarely, and flows open often.
const interfaceNetsTTL = 10 * time.Second

// interfaceAddrs lists the machine's interface addresses. Tests replace it.
var interfaceAddrs = net.InterfaceAddrs

var ifNets struct {
	mu      sync.Mutex
	nets    []netip.Prefix
	fetched time.Time
}

// interfaceNets returns the networks of the machine's interfaces. On an IPv6
// home network every device has a global address, so these are the only
// rule that keeps the home's /64 out of reach. Networks wider than /8 (IPv4)
// or /32 (IPv6) are skipped, so an odd tunnel address cannot block the whole
// internet.
func interfaceNets() []netip.Prefix {
	ifNets.mu.Lock()
	defer ifNets.mu.Unlock()
	if ifNets.nets != nil && time.Since(ifNets.fetched) < interfaceNetsTTL {
		return ifNets.nets
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		// Keep the last answer; the fixed ranges still apply.
		return ifNets.nets
	}
	nets := make([]netip.Prefix, 0, len(addrs))
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipnet.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		bits, _ := ipnet.Mask.Size()
		if ip.Is4() && bits > 32 {
			bits -= 96 // a 16-byte mask on an IPv4 address
		}
		if (ip.Is4() && bits < 8) || (ip.Is6() && bits < 32) {
			continue
		}
		pfx, err := ip.WithZone("").Prefix(bits)
		if err != nil {
			continue
		}
		nets = append(nets, pfx)
	}
	ifNets.nets, ifNets.fetched = nets, time.Now()
	return nets
}
