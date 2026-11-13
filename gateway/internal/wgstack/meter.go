package wgstack

import (
	"net/netip"
	"sync"
	"sync/atomic"
)

// PeerID identifies a peer to the meter (its WireGuard public key).
type PeerID [32]byte

// Meter counts inner IP bytes per peer. A peer's tunnel addresses share one
// counter, which survives address changes and lives until the peer is
// removed. Packets from or to addresses the meter does not know are refused,
// so nothing passes the gateway unmetered.
type Meter struct {
	mu     sync.RWMutex
	byAddr map[netip.Addr]*counter
	byPeer map[PeerID]*counter
}

type counter struct {
	up, down atomic.Uint64
	addrs    []netip.Addr
}

// NewMeter returns an empty meter.
func NewMeter() *Meter {
	return &Meter{byAddr: make(map[netip.Addr]*counter), byPeer: make(map[PeerID]*counter)}
}

// Set points a peer's tunnel addresses at its counter, creating the counter
// on first use and keeping it, with its counts, on later calls.
func (m *Meter) Set(peer PeerID, addrs ...netip.Addr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.byPeer[peer]
	if c == nil {
		c = &counter{}
		m.byPeer[peer] = c
	}
	for _, a := range c.addrs {
		delete(m.byAddr, a)
	}
	c.addrs = c.addrs[:0]
	for _, a := range addrs {
		if a.IsValid() {
			m.byAddr[a] = c
			c.addrs = append(c.addrs, a)
		}
	}
}

// Remove forgets a peer and returns its final counts.
func (m *Meter) Remove(peer PeerID) (up, down uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.byPeer[peer]
	if c == nil {
		return 0, 0
	}
	for _, a := range c.addrs {
		delete(m.byAddr, a)
	}
	delete(m.byPeer, peer)
	return c.up.Load(), c.down.Load()
}

// Usage returns the bytes counted for the peer that owns addr.
func (m *Meter) Usage(addr netip.Addr) (up, down uint64) {
	m.mu.RLock()
	c := m.byAddr[addr]
	m.mu.RUnlock()
	if c == nil {
		return 0, 0
	}
	return c.up.Load(), c.down.Load()
}

// countUp records a packet from a peer. It reports false for unknown sources.
func (m *Meter) countUp(src netip.Addr, n uint64) bool {
	m.mu.RLock()
	c := m.byAddr[src]
	m.mu.RUnlock()
	if c == nil {
		return false
	}
	c.up.Add(n)
	return true
}

// countDown records a packet to a peer. It reports false for unknown destinations.
func (m *Meter) countDown(dst netip.Addr, n uint64) bool {
	m.mu.RLock()
	c := m.byAddr[dst]
	m.mu.RUnlock()
	if c == nil {
		return false
	}
	c.down.Add(n)
	return true
}
