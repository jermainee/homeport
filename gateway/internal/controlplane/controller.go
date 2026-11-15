package controlplane

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jermainee/homeport/gateway"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// ConfigInterval is how often the controller polls the config, as the
// contract asks.
const ConfigInterval = 30 * time.Second

// ConfigRefreshInterval is the least time between two config fetches that
// RefreshConfig asks for, however many unknown agents or WireGuard keys
// connect.
const ConfigRefreshInterval = 2 * time.Second

// Gateway is the part of *gateway.Server the controller drives.
type Gateway interface {
	AddPeer(gateway.Peer) error
	RemovePeer(gateway.Key) (gateway.Usage, error)
	SetPeerLine(key gateway.Key, lineID string)
	SetPeerHTTPSOnly(key gateway.Key, on bool)
	AddLine(gateway.Line) error
	RemoveLine(id string)
	PeerUsage(addr netip.Addr) gateway.Usage
	ResetPeerFlows(gateway.Key)
	SuspendPeer(key gateway.Key, suspended bool)
}

// Clock tells the time. Tests move it by hand.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Options configures a Controller.
type Options struct {
	Clock  Clock        // nil means the system clock
	Logger *slog.Logger // nil means slog.Default()
}

// Controller runs a gateway from the control API.
type Controller struct {
	api   *Client
	clock Clock
	log   *slog.Logger

	refresh chan struct{} // RefreshConfig's request, at most one pending

	mu         sync.Mutex
	gw         Gateway
	etag       string // of the config applied in full; empty after a failed apply
	fetched    string // etag of the last config fetched, applied in full or not
	topUp      map[string]bool
	lines      map[string]ConfigLine
	peers      map[string]*peerState // by peer ID
	grace      time.Duration         // for a session without a lease: the last grace_seconds the API sent
	failback   time.Duration         // the config's failback_seconds
	events     []UsageEvent          // cut, not reported yet, oldest first
	afterFinal map[string][]leaseReturn
	returns    []leaseReturn  // leases to return now
	inflight   sync.WaitGroup // lease requests; Add only under mu and before closing
	closing    bool           // set by Close: no new sessions or lease requests
	flushMu    sync.Mutex     // one Flush at a time

	// lineMu guards what OnLineEvent changes. The gateway calls OnLineEvent
	// while the controller may hold mu (RemoveLine disconnects an agent), so
	// OnLineEvent never takes mu. Lock order: mu, then lineMu.
	lineMu     sync.Mutex
	lineIPs    map[string]netip.Addr // online lines and their agents' public IPs
	lineUp     map[string]time.Time  // online lines: since when, without a break
	lineDown   map[string]time.Time  // lines that went offline: since when
	lineEvents []LineEvent           // not reported yet, oldest first
}

type peerState struct {
	cfg            ConfigPeer
	peer           gateway.Peer
	reported       gateway.Usage // the peer's metered bytes that usage events hold
	lastSeen       gateway.Usage // the peer's metered bytes at the last step
	lastActive     time.Time
	suspendedUntil time.Time // set while the peer is cut off after a closed session
	sess           *session
}

type leaseReturn struct {
	leaseID, requestID string
}

// New returns a controller that talks to the API through api. Attach a
// gateway before running it.
func New(api *Client, opts Options) *Controller {
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Controller{
		api:        api,
		refresh:    make(chan struct{}, 1),
		clock:      opts.Clock,
		log:        opts.Logger,
		topUp:      map[string]bool{},
		lines:      map[string]ConfigLine{},
		peers:      map[string]*peerState{},
		grace:      defaultGrace,
		failback:   defaultFailback,
		afterFinal: map[string][]leaseReturn{},
		lineIPs:    map[string]netip.Addr{},
		lineUp:     map[string]time.Time{},
		lineDown:   map[string]time.Time{},
	}
}

// Attach sets the gateway the controller drives. The gateway's
// AccessControl and OnLineEvent are usually this controller, so it is
// attached after both exist.
func (c *Controller) Attach(gw Gateway) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gw = gw
}

// SyncConfig fetches the config once and applies what changed. On an error
// the gateway keeps its current config.
func (c *Controller) SyncConfig(ctx context.Context) error {
	c.mu.Lock()
	etag := c.etag
	c.mu.Unlock()
	return c.syncConfig(ctx, etag)
}

// syncRefresh is the fetch RefreshConfig asks for. It asks only for a
// config changed since the last fetch, even if applying that one failed: a
// refresh is for new lines and peers, and an unchanged config must cost a
// 304 however often unknown keys connect. The next poll retries the failed
// apply.
func (c *Controller) syncRefresh(ctx context.Context) error {
	c.mu.Lock()
	etag := c.fetched
	c.mu.Unlock()
	return c.syncConfig(ctx, etag)
}

// syncConfig fetches the config unless its etag is still etag, and applies
// what changed.
func (c *Controller) syncConfig(ctx context.Context, etag string) error {
	cfg, changed, err := c.api.Config(ctx, etag)
	if err != nil || !changed {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetched = cfg.ETag
	if err := c.applyLocked(cfg); err != nil {
		c.etag = "" // the next poll fetches and applies the whole config again
		return err
	}
	c.etag = cfg.ETag
	return nil
}

// RefreshConfig asks Run to fetch the config again now, because an agent
// or a WireGuard client with an unknown key connected: its line or peer may
// be newer than the gateway's config. Requests made before a fetch starts
// share that fetch. It never blocks, so the gateway may call it from a TLS
// handshake or the WireGuard receive path.
func (c *Controller) RefreshConfig() {
	select {
	case c.refresh <- struct{}{}:
	default: // a fetch is already due
	}
}

// applyLocked makes the gateway serve exactly cfg. Removals go first, so an
// address or pin can move from one peer or line to another.
func (c *Controller) applyLocked(cfg Config) error {
	var errs []error
	wantPeers := map[string]ConfigPeer{}
	for _, p := range cfg.Peers {
		wantPeers[p.PeerID] = p
	}
	wantLines := map[string]ConfigLine{}
	for _, l := range cfg.Lines {
		wantLines[l.LineID] = l
	}

	for id, ps := range c.peers {
		want, ok := wantPeers[id]
		if !ok || !sameDevice(want, ps.cfg) {
			c.removePeerLocked(ps)
			continue
		}
		// The same device on other lines stays on the gateway, WireGuard
		// session included. Step moves its session if its line is no
		// longer one of them. A changed HTTPS only applies in place; the
		// gateway resets the device's flows, so no unencrypted one
		// outlives turning it on.
		if want.HTTPSOnly != ps.cfg.HTTPSOnly {
			c.gw.SetPeerHTTPSOnly(ps.peer.PublicKey, want.HTTPSOnly)
			ps.peer.HTTPSOnly = want.HTTPSOnly
		}
		ps.cfg, ps.peer.LineID = want, want.LineID
	}
	for id, old := range c.lines {
		// A line whose pin changes is removed too: its agent is
		// disconnected either way, and its old pin may be another line's
		// new one.
		if want, ok := wantLines[id]; !ok || want.DeviceSPKISHA256 != old.DeviceSPKISHA256 {
			c.gw.RemoveLine(id)
			delete(c.lines, id)
		}
	}
	for id, l := range wantLines {
		if old, ok := c.lines[id]; ok && old == l {
			continue
		}
		line, err := parseLine(l)
		if err == nil {
			err = c.gw.AddLine(line)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("line %s: %w", id, err))
			continue
		}
		c.lines[id] = l
	}
	for id, p := range wantPeers {
		if _, ok := c.peers[id]; ok {
			continue
		}
		peer, err := parsePeer(p)
		if err == nil {
			err = c.gw.AddPeer(peer)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("peer %s: %w", id, err))
			continue
		}
		c.peers[id] = &peerState{cfg: p, peer: peer}
	}

	c.topUp = map[string]bool{}
	for _, h := range cfg.TopUpHostnames {
		c.topUp[normalizeHost(h)] = true
	}
	c.failback = time.Duration(cfg.FailbackSeconds) * time.Second
	c.lineMu.Lock()
	for id := range c.lineDown {
		if _, ok := wantLines[id]; !ok {
			delete(c.lineDown, id) // a line that left the config
		}
	}
	c.lineMu.Unlock()
	return errors.Join(errs...)
}

// removePeerLocked drops a peer from the gateway, open flows included, and
// ends its session with the peer's final metered usage.
func (c *Controller) removePeerLocked(ps *peerState) {
	u, err := c.gw.RemovePeer(ps.peer.PublicKey)
	if err != nil {
		c.log.Error("remove peer", "peer_id", ps.cfg.PeerID, "err", err)
		u = c.usageLocked(ps)
	}
	c.endSessionLocked(ps, u, c.clock.Now(), "peer removed")
	delete(c.peers, ps.cfg.PeerID)
}

func parsePeer(p ConfigPeer) (gateway.Peer, error) {
	key, err := gateway.ParseKey(p.PublicKey)
	if err != nil {
		return gateway.Peer{}, err
	}
	peer := gateway.Peer{ID: p.PeerID, PublicKey: key, MemberID: p.MemberID, LineID: p.LineID, HTTPSOnly: p.HTTPSOnly}
	if peer.IPv4, err = netip.ParseAddr(p.IPv4); err != nil || !peer.IPv4.Is4() {
		return gateway.Peer{}, fmt.Errorf("invalid IPv4 tunnel address %q", p.IPv4)
	}
	if peer.IPv6, err = netip.ParseAddr(p.IPv6); err != nil || !peer.IPv6.Is6() || peer.IPv6.Is4In6() {
		return gateway.Peer{}, fmt.Errorf("invalid IPv6 tunnel address %q", p.IPv6)
	}
	return peer, nil
}

// parseLine converts a config line. The contract sends the agent's SPKI
// hash as lowercase hex; relay-protocol's Pin holds the same 32 bytes.
func parseLine(l ConfigLine) (gateway.Line, error) {
	b, err := hex.DecodeString(l.DeviceSPKISHA256)
	if err != nil || len(b) != len(relayprotocol.Pin{}) {
		return gateway.Line{}, fmt.Errorf("device_spki_sha256 is not 64 hex characters")
	}
	return gateway.Line{ID: l.LineID, OwnerMemberID: l.OwnerMemberID, AgentPin: relayprotocol.Pin(b)}, nil
}

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// TopUpHost reports whether host is one of the config's top-up hosts.
func (c *Controller) TopUpHost(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.topUp[normalizeHost(host)]
}
