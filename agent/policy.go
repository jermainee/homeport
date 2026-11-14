package agent

import (
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
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
	// testOrigins are loopback test servers allowed in tests only, and
	// testRoutes send a destination to one of them.
	testOrigins []netip.AddrPort
	testRoutes  map[netip.AddrPort]netip.AddrPort
}

// newPolicy returns the policy for one connection. It drops the cached
// interface networks, so a machine that moved to another network refuses the
// new home network from the connection's first flow on.
func newPolicy(ownIP netip.Addr, testOrigins []netip.AddrPort, testRoutes map[netip.AddrPort]netip.AddrPort) *policy {
	ifNets.mu.Lock()
	ifNets.fetched = time.Time{} // refetch, but keep the last answer if that fails
	ifNets.mu.Unlock()
	return &policy{ownIP: ownIP.Unmap(), localNets: interfaceNets, testOrigins: testOrigins, testRoutes: testRoutes}
}

// target is the address a flow to dst connects to: dst itself, or in tests
// the loopback origin a test route sends dst to. The policy then checks the
// target, so a route reaches its origin and nothing else.
func (p *policy) target(dst netip.AddrPort) netip.AddrPort {
	dst = unmapPort(dst)
	if o, ok := p.testRoutes[dst]; ok {
		return o
	}
	return dst
}

func unmapPort(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
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

// check returns a *policyError if the policy refuses dst.
func (p *policy) check(dst netip.AddrPort) error {
	dst = unmapPort(dst)
	addr := dst.Addr()
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
	if relayprotocol.SpecialPurpose(addr) {
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

// allowsAnswer reports whether a DNS answer may hold addr: whether the
// policy would let a flow reach it on some port.
func (p *policy) allowsAnswer(addr netip.Addr) bool {
	for _, o := range p.testOrigins {
		if addr.Unmap() == o.Addr() {
			return true
		}
	}
	for dst := range p.testRoutes {
		if addr.Unmap() == dst.Addr() {
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

// homePrefixBits is the delegated prefix a home is assumed to number its
// IPv6 networks in. Most ISPs delegate a /56 (RIPE-690 recommends /48 or
// /56); a home splits it into /64s for its main, guest and IoT networks.
const homePrefixBits = 56

// globalUnicast is the IPv6 range ISPs number homes in.
var globalUnicast = netip.MustParsePrefix("2000::/3")

// interfaceNets returns the networks of the machine's interfaces. On an IPv6
// home network every device has a global address, so these are the only
// rule that keeps the home out of reach. Each global IPv6 address also
// stands for the /56 around it, which covers the home's other /64s (guest
// and IoT networks) and an address that DHCPv6 reports as a /128. Networks
// wider than /8 (IPv4) or /32 (IPv6) are skipped, so an odd tunnel address
// cannot block the whole internet.
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
		ip = ip.WithZone("")
		if ip.Is6() && globalUnicast.Contains(ip) {
			bits = min(bits, homePrefixBits)
		}
		pfx, err := ip.Prefix(bits)
		if err != nil {
			continue
		}
		nets = append(nets, pfx)
	}
	ifNets.nets, ifNets.fetched = nets, time.Now()
	return nets
}
