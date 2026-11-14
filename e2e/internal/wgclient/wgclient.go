// Package wgclient is a stock WireGuard client in userspace, as the official
// app or a travel router would run it: wireguard-go with a netstack, so it
// changes no routes on the test machine. Its tun counts every inner IP byte,
// which the gateway's per-peer meter must match exactly.
package wgclient

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sync/atomic"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// Config is a member's WireGuard config.
type Config struct {
	PrivateKey [32]byte
	Address    netip.Addr // the tunnel address
	DNS        netip.Addr // the gateway's virtual resolver; none if not valid
	MTU        int        // 0 means 1280

	Endpoint      string // the gateway's host:port
	PeerPublicKey string // the gateway's public key, in base64
}

// Client is a running WireGuard client.
type Client struct {
	// Net dials and resolves through the tunnel.
	Net *netstack.Net
	dev *device.Device
	tun *countingTun
}

// New starts a client. Its first packet starts the handshake.
func New(cfg Config) (*Client, error) {
	if cfg.MTU == 0 {
		cfg.MTU = 1280
	}
	peer, err := base64.StdEncoding.DecodeString(cfg.PeerPublicKey)
	if err != nil || len(peer) != 32 {
		return nil, errors.New("wgclient: the peer public key is not 32 bytes of base64")
	}
	var dns []netip.Addr
	if cfg.DNS.IsValid() {
		dns = append(dns, cfg.DNS)
	}
	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{cfg.Address}, dns, cfg.MTU)
	if err != nil {
		return nil, fmt.Errorf("wgclient: create tun: %w", err)
	}
	ct := &countingTun{Device: tunDev}
	dev := device.NewDevice(ct, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	uapi := fmt.Sprintf("private_key=%s\npublic_key=%s\nendpoint=%s\nallowed_ip=0.0.0.0/0\nallowed_ip=::/0\n",
		hex.EncodeToString(cfg.PrivateKey[:]), hex.EncodeToString(peer), cfg.Endpoint)
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wgclient: configure: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wgclient: up: %w", err)
	}
	return &Client{Net: tnet, dev: dev, tun: ct}, nil
}

// Sent is the inner IP bytes the client handed to WireGuard so far.
func (c *Client) Sent() uint64 { return c.tun.sent.Load() }

// Received is the inner IP bytes WireGuard delivered to the client so far.
func (c *Client) Received() uint64 { return c.tun.received.Load() }

// Close stops the client.
func (c *Client) Close() { c.dev.Close() }

// countingTun counts the inner IP bytes crossing the tun in each direction.
type countingTun struct {
	tun.Device
	sent     atomic.Uint64 // packets the client's stack hands to WireGuard
	received atomic.Uint64 // packets WireGuard delivers to the client's stack
}

func (c *countingTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	n, err := c.Device.Read(bufs, sizes, offset)
	for i := 0; i < n; i++ {
		c.sent.Add(uint64(sizes[i])) // #nosec G115 -- a packet size
	}
	return n, err
}

func (c *countingTun) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		if len(b) > offset {
			c.received.Add(uint64(len(b) - offset)) // #nosec G115 -- a packet size
		}
	}
	return c.Device.Write(bufs, offset)
}
