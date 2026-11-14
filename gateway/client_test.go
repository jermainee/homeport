package gateway_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/jermainee/homeport/gateway"
)

// countingTun counts the inner IP bytes a WireGuard client sends and receives.
// The gateway's per-peer meter must match these numbers exactly.
type countingTun struct {
	tun.Device
	sent     atomic.Uint64 // packets the client's stack hands to WireGuard
	received atomic.Uint64 // packets WireGuard delivers to the client's stack
}

func (c *countingTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	n, err := c.Device.Read(bufs, sizes, offset)
	for i := 0; i < n; i++ {
		c.sent.Add(uint64(len(bufs[i][offset : offset+sizes[i]])))
	}
	return n, err
}

func (c *countingTun) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		if len(b) > offset {
			c.received.Add(uint64(len(b[offset:])))
		}
	}
	return c.Device.Write(bufs, offset)
}

// testClient is a stock WireGuard client running in userspace, like the
// official app or a travel router would.
type testClient struct {
	net *netstack.Net
	tun *countingTun
	dev *device.Device
}

// newClient connects a client with key that sends from ip and any more
// tunnel addresses.
func newClient(t *testing.T, gw *gateway.Server, key gateway.Key, ip netip.Addr, more ...netip.Addr) *testClient {
	t.Helper()
	addrs := append([]netip.Addr{ip}, more...)
	tunDev, tnet, err := netstack.CreateNetTUN(addrs, []netip.Addr{netip.MustParseAddr("10.77.0.1")}, 1280)
	if err != nil {
		t.Fatalf("create client tun: %v", err)
	}
	ct := &countingTun{Device: tunDev}
	dev := device.NewDevice(ct, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	uapi := fmt.Sprintf("private_key=%s\npublic_key=%s\nendpoint=127.0.0.1:%d\nallowed_ip=0.0.0.0/0\nallowed_ip=::/0\n",
		hex.EncodeToString(key[:]), hex.EncodeToString(gw.PublicKey().Bytes()), gw.ListenPort())
	if err := dev.IpcSet(uapi); err != nil {
		t.Fatalf("configure client: %v", err)
	}
	if err := dev.Up(); err != nil {
		t.Fatalf("client up: %v", err)
	}
	t.Cleanup(dev.Close)
	return &testClient{net: tnet, tun: ct, dev: dev}
}

// handshakeWithin sends a datagram through the tunnel, which starts the
// client's handshake, and fails the test unless a handshake completes
// within limit. It returns how long that took. Like every stock client, it
// sends its handshake again every 5 s until one is answered.
func (c *testClient) handshakeWithin(t *testing.T, limit time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	uc, err := c.net.DialUDPAddrPort(netip.AddrPort{}, netip.MustParseAddrPort("11.22.33.44:9"))
	if err != nil {
		t.Fatalf("dial UDP: %v", err)
	}
	defer func() { _ = uc.Close() }()
	if _, err := uc.Write([]byte("hello")); err != nil {
		t.Fatalf("write UDP: %v", err)
	}
	for !c.handshaked(t) {
		if time.Since(start) > limit {
			t.Fatalf("no handshake within %v", limit)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return time.Since(start)
}

// handshaked reports whether the client has completed a handshake.
func (c *testClient) handshaked(t *testing.T) bool {
	t.Helper()
	state, err := c.dev.IpcGet()
	if err != nil {
		t.Fatalf("read client state: %v", err)
	}
	for line := range strings.Lines(state) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "last_handshake_time_sec="); ok {
			return v != "0"
		}
	}
	return false
}

func (c *testClient) get(t *testing.T, url string) []byte {
	t.Helper()
	httpClient := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{DialContext: c.net.DialContext, DisableKeepAlives: true},
	}
	resp, err := httpClient.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

// tryGet fetches url and reports any failure instead of failing the test.
func (c *testClient) tryGet(url string, timeout time.Duration) error {
	httpClient := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: c.net.DialContext, DisableKeepAlives: true},
	}
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.ReadAll(resp.Body)
	return err
}

// udpRoundTrip sends one datagram through the tunnel and returns the reply.
func (c *testClient) udpRoundTrip(t *testing.T, dst netip.AddrPort, payload []byte) []byte {
	t.Helper()
	uc, err := c.net.DialUDPAddrPort(netip.AddrPort{}, dst)
	if err != nil {
		t.Fatalf("dial UDP: %v", err)
	}
	defer func() { _ = uc.Close() }()
	if _, err := uc.Write(payload); err != nil {
		t.Fatalf("write UDP: %v", err)
	}
	_ = uc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	n, err := uc.Read(buf)
	if err != nil {
		t.Fatalf("read UDP: %v", err)
	}
	return buf[:n]
}

// toOrigin sends every flow to one local test server, whatever its
// destination, so tests can use public-looking addresses.
type toOrigin struct {
	tcp, udp string
}

func (d toOrigin) DialTCP(ctx context.Context, _ gateway.Flow) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.tcp)
}

func (d toOrigin) DialUDP(ctx context.Context, _ gateway.Flow) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "udp", d.udp)
}

// refuseAll refuses every flow, as an agent does for a blocked destination.
type refuseAll struct{}

func (refuseAll) DialTCP(context.Context, gateway.Flow) (net.Conn, error) {
	return nil, errors.New("refused")
}

func (refuseAll) DialUDP(context.Context, gateway.Flow) (net.Conn, error) {
	return nil, errors.New("refused")
}

// newUDPEcho starts a UDP server on loopback that echoes every datagram.
func newUDPEcho(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().String()
}

func newGateway(t *testing.T, dialer gateway.FlowDialer) *gateway.Server {
	t.Helper()
	return newGatewayWith(t, gateway.Config{Dialer: dialer})
}

// newGatewayWith starts a gateway with a fresh private key and cfg's other settings.
func newGatewayWith(t *testing.T, cfg gateway.Config) *gateway.Server {
	t.Helper()
	cfg.PrivateKey = newKey(t)
	gw, err := gateway.New(cfg)
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	return gw
}

// parkingDialer holds every TCP dial until released, like an agent that is
// slow to answer.
type parkingDialer struct {
	release chan struct{}
	once    sync.Once
	parked  atomic.Int32
}

func (d *parkingDialer) DialTCP(ctx context.Context, _ gateway.Flow) (net.Conn, error) {
	d.parked.Add(1)
	select {
	case <-d.release:
	case <-ctx.Done():
	}
	return nil, errors.New("released")
}

func (d *parkingDialer) DialUDP(context.Context, gateway.Flow) (net.Conn, error) {
	return nil, errors.New("no UDP")
}

func (d *parkingDialer) releaseAll() { d.once.Do(func() { close(d.release) }) }

// recordingDialer connects to target and tracks whether the gateway fully
// closed every upstream connection it opened (CloseWrite alone does not count).
type recordingDialer struct {
	target string
	mu     sync.Mutex
	conns  []*recordedConn
}

func (d *recordingDialer) DialTCP(ctx context.Context, _ gateway.Flow) (net.Conn, error) {
	var nd net.Dialer
	c, err := nd.DialContext(ctx, "tcp", d.target)
	if err != nil {
		return nil, err
	}
	rc := &recordedConn{TCPConn: c.(*net.TCPConn)}
	d.mu.Lock()
	d.conns = append(d.conns, rc)
	d.mu.Unlock()
	return rc, nil
}

func (d *recordingDialer) DialUDP(context.Context, gateway.Flow) (net.Conn, error) {
	return nil, errors.New("no UDP")
}

// allClosed reports whether every dialed connection was fully closed.
func (d *recordingDialer) allClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		if !c.closed.Load() {
			return false
		}
	}
	return len(d.conns) > 0
}

type recordedConn struct {
	*net.TCPConn
	closed atomic.Bool
}

func (c *recordedConn) Close() error {
	c.closed.Store(true)
	return c.TCPConn.Close()
}

func newKey(t *testing.T) gateway.Key {
	t.Helper()
	k, err := gateway.GenerateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return k
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// waitUntil polls until cond holds.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitStable polls until read returns the same value for quiet, so in-flight
// packets (final ACKs, FINs) have landed before counters are compared.
func waitStable[T comparable](t *testing.T, quiet time.Duration, read func() T) T {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := read()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		v := read()
		if v != last {
			last, stableSince = v, time.Now()
			continue
		}
		if time.Since(stableSince) >= quiet {
			return v
		}
	}
	t.Fatalf("value did not settle: %v", last)
	return last
}
