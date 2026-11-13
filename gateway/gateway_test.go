package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
)

type counters struct {
	usage                      gateway.Usage
	clientSent, clientReceived uint64
}

func TestFetchThroughGatewayIsMeteredExactly(t *testing.T) {
	body := randomBytes(1 << 20)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(origin.Close)

	gw := newGateway(t, toOrigin{tcp: origin.Listener.Addr().String()})
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peerIP)

	got := client.get(t, "http://203.0.113.10/")
	if sum(got) != sum(body) {
		t.Fatalf("body differs: got %d bytes", len(got))
	}

	c := waitStable(t, 300*time.Millisecond, func() counters {
		return counters{gw.PeerUsage(peerIP), client.tun.sent.Load(), client.tun.received.Load()}
	})
	if c.usage.Up != c.clientSent {
		t.Errorf("gateway counted %d bytes up, client sent %d", c.usage.Up, c.clientSent)
	}
	if c.usage.Down != c.clientReceived {
		t.Errorf("gateway counted %d bytes down, client received %d", c.usage.Down, c.clientReceived)
	}
	if c.usage.Down < uint64(len(body)) {
		t.Errorf("down %d is less than the %d-byte body", c.usage.Down, len(body))
	}
}

func TestRemovedPeerIsCutOff(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hello"))
	}))
	t.Cleanup(origin.Close)
	gw := newGateway(t, toOrigin{tcp: origin.Listener.Addr().String()})
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peerIP)
	if got := string(client.get(t, "http://203.0.113.10/")); got != "hello" {
		t.Fatalf("before removal: got %q", got)
	}

	if err := gw.RemovePeer(key.PublicKey()); err != nil {
		t.Fatalf("remove peer: %v", err)
	}

	if err := client.tryGet("http://203.0.113.10/", 2*time.Second); err == nil {
		t.Fatal("removed peer could still fetch")
	}
}

func TestRefusedFlowResetsTheClientAtOnce(t *testing.T) {
	gw := newGateway(t, refuseAll{})
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peerIP)

	start := time.Now()
	err := client.tryGet("http://203.0.113.10/", 5*time.Second)
	if err == nil {
		t.Fatal("refused flow returned a response")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("refusal took %v; the client should get a reset, not a timeout", took)
	}
}

func TestUDPFlowIsRelayedAndMetered(t *testing.T) {
	echo := newUDPEcho(t)
	gw := newGateway(t, toOrigin{udp: echo})
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peerIP)

	reply := client.udpRoundTrip(t, netip.MustParseAddrPort("203.0.113.10:7"), []byte("ping"))
	if string(reply) != "ping" {
		t.Fatalf("got %q, want the echo", reply)
	}

	c := waitStable(t, 300*time.Millisecond, func() counters {
		return counters{gw.PeerUsage(peerIP), client.tun.sent.Load(), client.tun.received.Load()}
	})
	if c.usage.Up == 0 || c.usage.Up != c.clientSent || c.usage.Down != c.clientReceived {
		t.Errorf("usage %+v, client sent %d and received %d", c.usage, c.clientSent, c.clientReceived)
	}
}
