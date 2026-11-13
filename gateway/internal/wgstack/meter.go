package wgstack

import (
	"net/netip"
	"sync"
	"sync/atomic"
)

// Meter counts inner IP bytes per peer. A peer's IPv4 and IPv6 tunnel
// addresses share one counter. Packets from or to addresses the meter does
// not know are refused, so nothing passes the gateway unmetered.
type Meter struct {
	mu    sync.RWMutex
	peers map[netip.Addr]*counter
}

type counter struct {
	up, down atomic.Uint64
}

// NewMeter returns an empty meter.
func NewMeter() *Meter {
	return &Meter{peers: make(map[netip.Addr]*counter)}
}

// Register starts counting for one peer's tunnel addresses.
func (m *Meter) Register(addrs ...netip.Addr) {
	c := &counter{}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range addrs {
		if a.IsValid() {
			m.peers[a] = c
		}
	}
}

// Unregister stops counting for these addresses; later packets are refused.
func (m *Meter) Unregister(addrs ...netip.Addr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range addrs {
		delete(m.peers, a)
	}
}

// Usage returns the bytes counted for the peer that owns addr.
func (m *Meter) Usage(addr netip.Addr) (up, down uint64) {
	m.mu.RLock()
	c := m.peers[addr]
	m.mu.RUnlock()
	if c == nil {
		return 0, 0
	}
	return c.up.Load(), c.down.Load()
}

// countUp records a packet from a peer. It reports false for unknown sources.
func (m *Meter) countUp(src netip.Addr, n int) bool {
	m.mu.RLock()
	c := m.peers[src]
	m.mu.RUnlock()
	if c == nil {
		return false
	}
	c.up.Add(uint64(n))
	return true
}

// countDown records a packet to a peer. It reports false for unknown destinations.
func (m *Meter) countDown(dst netip.Addr, n int) bool {
	m.mu.RLock()
	c := m.peers[dst]
	m.mu.RUnlock()
	if c == nil {
		return false
	}
	c.down.Add(uint64(n))
	return true
}
