package gateway_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/tuntest"

	"github.com/jermainee/homeport/gateway"
)

// initiator makes WireGuard handshake initiations to one gateway key with
// wireguard-go's own code, as a stock client sends them.
type initiator struct {
	dev  *device.Device
	peer *device.Peer
	macs device.CookieGenerator
}

func newInitiator(t *testing.T, gatewayKey gateway.Key) *initiator {
	t.Helper()
	dev := device.NewDevice(tuntest.NewChannelTUN().TUN(), conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	t.Cleanup(dev.Close)
	if err := dev.IpcSet(fmt.Sprintf("public_key=%s\n", hex.EncodeToString(gatewayKey[:]))); err != nil {
		t.Fatalf("configure initiator: %v", err)
	}
	in := &initiator{dev: dev, peer: dev.LookupPeer(device.NoisePublicKey(gatewayKey))}
	in.macs.Init(device.NoisePublicKey(gatewayKey))
	return in
}

// from returns a handshake initiation from private key key.
func (in *initiator) from(key gateway.Key) ([]byte, error) {
	if err := in.dev.IpcSet(fmt.Sprintf("private_key=%s\n", hex.EncodeToString(key[:]))); err != nil {
		return nil, err
	}
	msg, err := in.dev.CreateMessageInitiation(in.peer)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := binary.Write(&b, binary.LittleEndian, msg); err != nil {
		return nil, err
	}
	pkt := b.Bytes()
	in.macs.AddMacs(pkt)
	return pkt, nil
}

// fromNewKey returns a handshake initiation from a key no one has used.
func (in *initiator) fromNewKey() ([]byte, error) {
	key, err := gateway.GenerateKey()
	if err != nil {
		return nil, err
	}
	return in.from(key)
}

// dialWireGuard returns a UDP socket connected to the gateway's WireGuard port.
func dialWireGuard(t *testing.T, gw *gateway.Server) net.Conn {
	t.Helper()
	c, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(gw.ListenPort()))))
	if err != nil {
		t.Fatalf("dial WireGuard: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestOnUnknownPeerIsCalledOnlyForKeysThatAreNoPeers(t *testing.T) {
	var calls atomic.Int32
	gw := newGatewayWith(t, gateway.Config{
		Dialer:        refuseAll{},
		OnUnknownPeer: func() { calls.Add(1) },
	})
	ip := netip.MustParseAddr("10.77.0.2")
	member := newKey(t)
	if err := gw.AddPeer(gateway.Peer{PublicKey: member.PublicKey(), IPv4: ip}); err != nil {
		t.Fatal(err)
	}
	newClient(t, gw, member, ip).handshakeWithin(t, 2*time.Second)

	udp := dialWireGuard(t, gw)
	stranger := newKey(t)
	// An initiation for another gateway's key is not this gateway's business.
	other, err := newInitiator(t, newKey(t).PublicKey()).from(stranger)
	if err != nil {
		t.Fatal(err)
	}
	mine, err := newInitiator(t, gw.PublicKey()).from(stranger)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkt := range [][]byte{other, mine} {
		if _, err := udp.Write(pkt); err != nil {
			t.Fatal(err)
		}
	}

	waitUntil(t, func() bool { return calls.Load() > 0 })
	if n := waitStable(t, 200*time.Millisecond, calls.Load); n != 1 {
		t.Errorf("OnUnknownPeer called %d times; want once, for the stranger only", n)
	}
}
