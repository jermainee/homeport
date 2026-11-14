package gateway_test

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// Two members' devices, as the API numbers them: one host number in both
// tunnel networks, and 10.77.0.1 (fd77::1) for the gateway.
var (
	peerA = gateway.Peer{ID: "peer-a", IPv4: netip.MustParseAddr("10.77.0.2"), IPv6: netip.MustParseAddr("fd77::2"), MemberID: "member-a"}
	peerB = gateway.Peer{ID: "peer-b", IPv4: netip.MustParseAddr("10.77.0.3"), IPv6: netip.MustParseAddr("fd77::3"), MemberID: "member-b"}
)

// unreachable are destinations no member may open a flow to: other
// members, the gateway itself (except DNS on the virtual resolver), and
// every network a home can number its devices in.
//
// The gateway's stack drops packets to loopback and multicast destinations
// before they become flows (dropped), so a TCP client gets no reset there.
var unreachable = []struct {
	name, dst string
	dropped   bool
}{
	{"another member", "10.77.0.3:8080", false},
	{"another member over IPv6", "[fd77::3]:8080", false},
	{"another member through NAT64", "[64:ff9b::a4d:3]:8080", false},
	{"an unassigned tunnel address", "10.77.200.9:8080", false},
	{"the gateway on another port", "10.77.0.1:80", false},
	{"the gateway's SSH port", "10.77.0.1:22", false},
	{"the gateway over IPv6", "[fd77::1]:53", false},
	{"a home router", "192.168.178.1:80", false},
	{"cloud metadata", "169.254.169.254:80", false},
	{"loopback", "127.0.0.1:80", true},
	{"carrier-grade NAT", "100.64.0.1:80", false},
	{"a fake-IP proxy", "198.18.0.1:80", false},
	{"documentation", "203.0.113.10:80", false},
	{"IPv6 link-local", "[fe80::1]:80", false},
	{"IPv6 unique local", "[fd00::1]:80", false},
	{"multicast", "239.255.255.250:1900", true},
}

// flowLog records every flow that reaches it and connects it to an origin.
type flowLog struct {
	next  gateway.FlowDialer
	mu    sync.Mutex
	flows []gateway.Flow
}

func (d *flowLog) DialTCP(ctx context.Context, f gateway.Flow) (net.Conn, error) {
	d.add(f)
	return d.next.DialTCP(ctx, f)
}

func (d *flowLog) DialUDP(ctx context.Context, f gateway.Flow) (net.Conn, error) {
	d.add(f)
	return d.next.DialUDP(ctx, f)
}

func (d *flowLog) add(f gateway.Flow) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flows = append(d.flows, f)
}

func (d *flowLog) all() []gateway.Flow {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]gateway.Flow(nil), d.flows...)
}

// addPeer adds p with a fresh key and connects its client.
func addPeer(t *testing.T, gw *gateway.Server, p gateway.Peer) *testClient {
	t.Helper()
	key := newKey(t)
	p.PublicKey = key.PublicKey()
	if err := gw.AddPeer(p); err != nil {
		t.Fatalf("add peer %s: %v", p.ID, err)
	}
	return newClient(t, gw, key, p.IPv4, p.IPv6)
}

// dialRefusedQuickly checks that a TCP connection to dst is refused with a
// reset instead of opening or timing out.
func dialRefusedQuickly(t *testing.T, c *testClient, dst netip.AddrPort) {
	t.Helper()
	start := time.Now()
	dialFails(t, c, dst, 5*time.Second)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("refusing TCP to %s took %v; the client should get a reset, not a timeout", dst, took)
	}
}

// dialFails checks that a TCP connection to dst does not open within timeout.
func dialFails(t *testing.T, c *testClient, dst netip.AddrPort, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := c.net.DialContextTCPAddrPort(ctx, dst)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("TCP to %s opened; it should have been refused", dst)
	}
}

func TestFlowsToUnreachableDestinationsNeverLeaveTheGateway(t *testing.T) {
	origins := &flowLog{next: toOrigin{tcp: newHoldingOrigin(t), udp: newUDPEcho(t)}}
	gw := newGateway(t, origins)
	a := addPeer(t, gw, peerA)

	for _, tc := range unreachable {
		t.Run(tc.name, func(t *testing.T) {
			dst := netip.MustParseAddrPort(tc.dst)
			if tc.dropped {
				dialFails(t, a, dst, time.Second)
			} else {
				dialRefusedQuickly(t, a, dst)
			}
			if err := a.tryUDP(dst, 300*time.Millisecond); err == nil {
				t.Errorf("UDP to %s was answered", dst)
			}
		})
	}
	if flows := origins.all(); len(flows) != 0 {
		t.Fatalf("flows to unreachable destinations reached the dialer: %+v", flows)
	}

	// The virtual resolver's DNS port and public destinations still work.
	if got := a.udpRoundTrip(t, netip.MustParseAddrPort("10.77.0.1:53"), []byte("query")); string(got) != "query" {
		t.Errorf("DNS through the virtual resolver answered %q", got)
	}
	a.dialHeld(t)
	if got := a.udpRoundTrip(t, netip.MustParseAddrPort("[2a00:1450:4001::1]:443"), []byte("quic")); string(got) != "quic" {
		t.Errorf("UDP to a public IPv6 address answered %q", got)
	}
}

func TestMembersCannotReachEachOther(t *testing.T) {
	gw := newGateway(t, toOrigin{tcp: newHoldingOrigin(t), udp: newUDPEcho(t)})
	a := addPeer(t, gw, peerA)
	b := addPeer(t, gw, peerB)

	// B listens on its tunnel addresses, as a device with open ports does.
	accepted := make(chan string, 8)
	for _, addr := range []netip.Addr{peerB.IPv4, peerB.IPv6} {
		ln, err := b.net.ListenTCPAddrPort(netip.AddrPortFrom(addr, 8080))
		if err != nil {
			t.Fatalf("B listens on %s: %v", addr, err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				accepted <- c.RemoteAddr().String()
				_ = c.Close()
			}
		}()
		uc, err := b.net.ListenUDPAddrPort(netip.AddrPortFrom(addr, 9000))
		if err != nil {
			t.Fatalf("B listens for UDP on %s: %v", addr, err)
		}
		t.Cleanup(func() { _ = uc.Close() })
		go func() {
			buf := make([]byte, 1500)
			for {
				_, from, err := uc.ReadFrom(buf)
				if err != nil {
					return
				}
				accepted <- from.String()
			}
		}()
	}
	usageB := gw.PeerUsage(peerB.IPv4)

	for _, addr := range []netip.Addr{peerB.IPv4, peerB.IPv6} {
		dialRefusedQuickly(t, a, netip.AddrPortFrom(addr, 8080))
		if err := a.tryUDP(netip.AddrPortFrom(addr, 9000), 300*time.Millisecond); err == nil {
			t.Errorf("UDP from A to B at %s was answered", addr)
		}
	}

	select {
	case from := <-accepted:
		t.Fatalf("B received a connection or datagram from %s", from)
	case <-time.After(200 * time.Millisecond):
	}
	if got := gw.PeerUsage(peerB.IPv4); got != usageB {
		t.Errorf("B's usage moved from %+v to %+v while A tried to reach it", usageB, got)
	}
}

func TestTwoMembersOnTheSameLineSeeNothingOfEachOther(t *testing.T) {
	gw := relayGateway(t)
	cert, pin := newAgentCert(t)
	if err := gw.AddLine(gateway.Line{ID: "line-1", OwnerMemberID: "member-owner", AgentPin: pin}); err != nil {
		t.Fatalf("add line: %v", err)
	}
	agent, err := startAgent(t, gw, cert, relayTo(newHoldingOrigin(t)))
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	onLine := func(p gateway.Peer) gateway.Peer { p.LineID = "line-1"; return p }
	a := addPeer(t, gw, onLine(peerA))
	addPeer(t, gw, onLine(peerB))

	for _, dst := range []string{"10.77.0.3:8080", "[fd77::3]:8080", "10.77.0.1:80"} {
		dialRefusedQuickly(t, a, netip.MustParseAddrPort(dst))
	}
	select {
	case open := <-agent.opens:
		t.Fatalf("a flow from A to B or the gateway reached the line's agent: %+v", open)
	case <-time.After(200 * time.Millisecond):
	}

	// A's own traffic goes through the shared line, carrying nothing that
	// names A or B, and counts for A alone.
	a.dialHeld(t)
	open := <-agent.opens
	if open != (relayprotocol.FlowOpen{Kind: relayprotocol.KindTCP, Dst: netip.MustParseAddrPort("11.22.33.44:80")}) {
		t.Errorf("the agent got %+v", open)
	}
	if up := gw.PeerUsage(peerA.IPv4).Up; up == 0 {
		t.Error("A's traffic was not counted for A")
	}
	if got := gw.PeerUsage(peerB.IPv4); got != (gateway.Usage{}) {
		t.Errorf("B was charged %+v for A's traffic", got)
	}
}

func TestPacketsWithAForgedSourceAreDropped(t *testing.T) {
	origins := &flowLog{next: toOrigin{tcp: newHoldingOrigin(t), udp: newUDPEcho(t)}}
	gw := newGateway(t, origins)
	addPeer(t, gw, peerB)
	keyA := newKey(t)
	a := peerA
	a.PublicKey = keyA.PublicKey()
	if err := gw.AddPeer(a); err != nil {
		t.Fatalf("add A: %v", err)
	}

	// A's key, sending as B, and as an address no peer has.
	for _, src := range []netip.Addr{peerB.IPv4, netip.MustParseAddr("10.77.0.99")} {
		forger := newClient(t, gw, keyA, src)
		if err := forger.tryGet("http://11.22.33.44/", time.Second); err == nil {
			t.Errorf("a request sent from %s with A's key was answered", src)
		}
		if err := forger.tryUDP(netip.MustParseAddrPort("10.77.0.1:53"), 300*time.Millisecond); err == nil {
			t.Errorf("DNS sent from %s with A's key was answered", src)
		}
	}

	if flows := origins.all(); len(flows) != 0 {
		t.Errorf("forged packets opened flows: %+v", flows)
	}
	for _, p := range []gateway.Peer{peerA, peerB} {
		if got := gw.PeerUsage(p.IPv4); got != (gateway.Usage{}) {
			t.Errorf("%s was charged %+v for forged packets", p.ID, got)
		}
	}
}

func TestPingsToOtherMembersAndTheGatewayGetNoAnswer(t *testing.T) {
	gw := newGateway(t, toOrigin{tcp: newHoldingOrigin(t), udp: newUDPEcho(t)})
	a := addPeer(t, gw, peerA)
	addPeer(t, gw, peerB)

	for _, tc := range []struct {
		src, dst netip.Addr
		echo     byte // ICMP or ICMPv6 echo request type
	}{
		{peerA.IPv4, peerB.IPv4, 8},
		{peerA.IPv4, netip.MustParseAddr("10.77.0.1"), 8},
		{peerA.IPv6, peerB.IPv6, 128},
		{peerA.IPv6, netip.MustParseAddr("fd77::1"), 128},
	} {
		pc, err := a.net.DialPingAddr(tc.src, tc.dst)
		if err != nil {
			t.Fatalf("ping socket to %s: %v", tc.dst, err)
		}
		// Type, code, checksum, identifier, sequence number; the ping
		// socket fills in the identifier and checksum.
		if _, err := pc.Write([]byte{tc.echo, 0, 0, 0, 0, 0, 0, 1, 'h', 'i'}); err != nil {
			t.Fatalf("ping %s: %v", tc.dst, err)
		}
		_ = pc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		if n, err := pc.Read(make([]byte, 1500)); err == nil {
			t.Errorf("a ping to %s was answered with %d bytes", tc.dst, n)
		}
		_ = pc.Close()
	}
}
