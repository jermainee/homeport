package gateway_test

import (
	"context"
	"net"
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
		_, _ = w.Write(body)
	}))
	t.Cleanup(origin.Close)

	gw := newGateway(t, toOrigin{tcp: origin.Listener.Addr().String()})
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peerIP)

	got := client.get(t, "http://11.22.33.44/")
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
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(origin.Close)
	gw := newGateway(t, toOrigin{tcp: origin.Listener.Addr().String()})
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peerIP)
	if got := string(client.get(t, "http://11.22.33.44/")); got != "hello" {
		t.Fatalf("before removal: got %q", got)
	}

	if _, err := gw.RemovePeer(key.PublicKey()); err != nil {
		t.Fatalf("remove peer: %v", err)
	}

	if err := client.tryGet("http://11.22.33.44/", 2*time.Second); err == nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := client.net.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort("11.22.33.44:80"))
	if err == nil {
		_ = conn.Close()
		t.Fatal("the handshake completed; a refused flow must be reset before it opens")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("refusal took %v; the client should get a reset, not a timeout", took)
	}
}

func TestNewRejectsAMissingPrivateKey(t *testing.T) {
	if _, err := gateway.New(gateway.Config{Dialer: refuseAll{}}); err == nil {
		t.Fatal("New accepted an all-zero WireGuard private key")
	}
}

func TestReAddingAPeerKeepsItsUsage(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(origin.Close)
	gw := newGateway(t, toOrigin{tcp: origin.Listener.Addr().String()})
	key := newKey(t)
	peer := gateway.Peer{PublicKey: key.PublicKey(), IPv4: netip.MustParseAddr("10.77.0.2")}
	if err := gw.AddPeer(peer); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peer.IPv4)
	client.get(t, "http://11.22.33.44/")
	before := waitStable(t, 300*time.Millisecond, func() gateway.Usage { return gw.PeerUsage(peer.IPv4) })

	if err := gw.AddPeer(peer); err != nil {
		t.Fatalf("re-add peer: %v", err)
	}

	if after := gw.PeerUsage(peer.IPv4); after != before {
		t.Fatalf("usage changed from %+v to %+v when the peer was re-added", before, after)
	}
}

func TestAddPeerRejectsATunnelAddressInUse(t *testing.T) {
	gw := newGateway(t, refuseAll{})
	ip := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: newKey(t).PublicKey(), IPv4: ip}); err != nil {
		t.Fatalf("add first peer: %v", err)
	}
	if err := gw.AddPeer(gateway.Peer{PublicKey: newKey(t).PublicKey(), IPv4: ip}); err == nil {
		t.Fatal("a second peer was given a tunnel address that is already in use")
	}
}

func TestRemovePeerReturnsItsFinalUsage(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(randomBytes(64 << 10))
	}))
	t.Cleanup(origin.Close)
	gw := newGateway(t, toOrigin{tcp: origin.Listener.Addr().String()})
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peerIP)
	client.get(t, "http://11.22.33.44/")
	c := waitStable(t, 300*time.Millisecond, func() counters {
		return counters{gw.PeerUsage(peerIP), client.tun.sent.Load(), client.tun.received.Load()}
	})

	final, err := gw.RemovePeer(key.PublicKey())
	if err != nil {
		t.Fatalf("remove peer: %v", err)
	}

	if final.Up != c.clientSent || final.Down != c.clientReceived {
		t.Fatalf("RemovePeer returned %+v; the client sent %d and received %d", final, c.clientSent, c.clientReceived)
	}
}

func TestPendingFlowsAreCappedPerPeer(t *testing.T) {
	dialer := &parkingDialer{release: make(chan struct{})}
	gw := newGatewayWith(t, gateway.Config{Dialer: dialer, MaxPendingFlowsPerPeer: 2})
	t.Cleanup(dialer.releaseAll)
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peerIP)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for port := uint16(1); port <= 2; port++ {
		go func() {
			_, _ = client.net.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(netip.MustParseAddr("11.22.33.44"), port))
		}()
	}
	waitUntil(t, func() bool { return dialer.parked.Load() == 2 })

	start := time.Now()
	conn, err := client.net.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort("11.22.33.44:3"))
	if err == nil {
		_ = conn.Close()
		t.Fatal("a third pending flow was accepted")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the third flow waited %v; it should be refused at once", took)
	}
}

func TestCloseClosesUpstreamConnections(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	gotByte := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
			go func() {
				// A silent origin: reads what the client sends, never writes, never closes.
				if _, err := c.Read(make([]byte, 1)); err == nil {
					gotByte <- struct{}{}
				}
			}()
		}
	}()
	dialer := &recordingDialer{target: ln.Addr().String()}
	gw := newGateway(t, dialer)
	key := newKey(t)
	peerIP := netip.MustParseAddr("10.77.0.2")
	if err := gw.AddPeer(gateway.Peer{PublicKey: key.PublicKey(), IPv4: peerIP}); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	client := newClient(t, gw, key, peerIP)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := client.net.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort("11.22.33.44:80"))
	if err != nil {
		t.Fatalf("dial through gateway: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("write through gateway: %v", err)
	}
	select {
	case <-gotByte: // the relay is running end to end
	case <-time.After(5 * time.Second):
		t.Fatal("the origin never received the client's byte")
	}

	_ = gw.Close()

	deadline := time.Now().Add(3 * time.Second)
	for !dialer.allClosed() {
		if time.Now().After(deadline) {
			t.Fatal("an upstream connection was still open 3 s after Close")
		}
		time.Sleep(20 * time.Millisecond)
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

	reply := client.udpRoundTrip(t, netip.MustParseAddrPort("11.22.33.44:7"), []byte("ping"))
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
