// Package agent is Homeport's sharing agent. It runs on a member's home
// machine, dials out to an edge gateway over QUIC and opens a real socket on
// the member's line for every flow the gateway relays to it.
//
// The wire format is relay-protocol (see its README). The agent refuses flows
// into the member's home network and to mail ports (see policy.go), pauses
// while another VPN carries the machine's default route, and logs session
// metadata only: never a destination, a DNS name or content.
package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"

	"github.com/jermainee/homeport/agent/internal/netwatch"
)

// Version is the agent release reported to the gateway when Config.Version
// is empty.
const Version = "0.1.0"

// Defaults for the zero values in Config.
const (
	DefaultStatsInterval       = 30 * time.Second
	DefaultReconnectMin        = time.Second
	DefaultReconnectMax        = time.Minute
	DefaultTunnelCheckInterval = time.Second
)

// Config configures an Agent.
type Config struct {
	// GatewayAddr is the gateway's QUIC address as host:port.
	GatewayAddr string
	// GatewayPin is the SPKI pin of the gateway's certificate. The agent
	// accepts no other gateway.
	GatewayPin relayprotocol.Pin
	// Certificate is the agent's identity: a self-signed Ed25519
	// certificate from relayprotocol.GenerateSelfSigned or NewSelfSigned,
	// whose pin the gateway knows.
	Certificate tls.Certificate
	// Version is the release sent in Hello. Empty means [Version].
	Version string

	// Resolver answers DNS flows. Nil means net.DefaultResolver, the
	// system resolver, so websites see the home ISP's resolver.
	Resolver Resolver
	// Logger receives session metadata only. Nil discards logs.
	Logger *slog.Logger

	// StatsInterval is how often the agent sends its counters to the
	// gateway. Zero means DefaultStatsInterval.
	StatsInterval time.Duration
	// ReconnectMin and ReconnectMax bound the delay between connection
	// attempts, which doubles after each failure and is jittered. Zero
	// means DefaultReconnectMin and DefaultReconnectMax.
	ReconnectMin, ReconnectMax time.Duration

	// TunnelCheck reports whether another VPN carries the machine's
	// default route, in which case sharing pauses so shared traffic never
	// leaves through someone else's tunnel. Nil means the OS check: a
	// default route through utun on macOS, and none yet elsewhere.
	TunnelCheck func() (bool, error)
	// TunnelCheckInterval is how often TunnelCheck runs. Zero means
	// DefaultTunnelCheckInterval.
	TunnelCheckInterval time.Duration

	// testOrigins are loopback addresses the policy lets through. Only
	// this package's tests and e2e builds (policy_testallow.go) set them.
	testOrigins []netip.AddrPort
}

// State is what the agent is doing.
type State int

const (
	// StateStopped means Run is not running.
	StateStopped State = iota
	// StateConnecting means the agent is dialing the gateway or waiting
	// to retry.
	StateConnecting
	// StateConnected means the gateway acknowledged the agent and may
	// relay flows to it.
	StateConnected
	// StatePaused means sharing is paused, by Pause or because another
	// VPN carries the default route. The agent holds no connection.
	StatePaused
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StatePaused:
		return "paused"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// Stats is a snapshot of the agent. Its counters cover every connection
// since the Agent was created.
type Stats struct {
	State State
	// LineID is the line the gateway acknowledged on the current or last
	// connection.
	LineID string
	// Paused reports a Pause without a later Resume.
	Paused bool
	// OtherVPN reports that another VPN carries the default route.
	OtherVPN bool
	// LastError is why the last connection attempt or connection ended,
	// or nil. It never names a destination.
	LastError error

	FlowsActive uint64
	FlowsTotal  uint64
	// BytesOut and BytesIn count payload sent to and received from
	// destinations.
	BytesOut uint64
	BytesIn  uint64
}

// Agent shares the member's line with one gateway. Create it with New and
// start it with Run.
type Agent struct {
	cfg Config
	log *slog.Logger

	running atomic.Bool
	wake    chan struct{} // nudges Run after Pause, Resume or a VPN change

	routeCheckFailing bool // only the goroutine running checkTunnel uses it

	mu         sync.Mutex
	state      State
	lineID     string
	paused     bool
	otherVPN   bool
	lastErr    error
	endSession context.CancelCauseFunc // ends the current connection, or nil
	past       counterValues           // counters of finished connections
	cur        *counters               // counters of the current connection, or nil
}

// New checks cfg and returns an agent that is not running yet.
func New(cfg Config) (*Agent, error) {
	switch {
	case cfg.GatewayAddr == "":
		return nil, errors.New("agent: Config.GatewayAddr is empty")
	case cfg.GatewayPin == relayprotocol.Pin{}:
		return nil, errors.New("agent: Config.GatewayPin is empty")
	case len(cfg.Certificate.Certificate) == 0 || cfg.Certificate.PrivateKey == nil:
		return nil, errors.New("agent: Config.Certificate is empty")
	}
	origins := make([]netip.AddrPort, len(cfg.testOrigins))
	for i, o := range cfg.testOrigins {
		if !o.Addr().IsLoopback() {
			return nil, errors.New("agent: test origins must be loopback addresses")
		}
		// The policy compares unmapped addresses.
		origins[i] = netip.AddrPortFrom(o.Addr().Unmap(), o.Port())
	}
	cfg.testOrigins = origins
	if cfg.Version == "" {
		cfg.Version = Version
	}
	if cfg.Resolver == nil {
		cfg.Resolver = systemResolver
	}
	if cfg.StatsInterval <= 0 {
		cfg.StatsInterval = DefaultStatsInterval
	}
	if cfg.ReconnectMin <= 0 {
		cfg.ReconnectMin = DefaultReconnectMin
	}
	if cfg.ReconnectMax <= 0 {
		cfg.ReconnectMax = DefaultReconnectMax
	}
	cfg.ReconnectMax = max(cfg.ReconnectMax, cfg.ReconnectMin)
	if cfg.TunnelCheck == nil {
		cfg.TunnelCheck = netwatch.Tunneled
	}
	if cfg.TunnelCheckInterval <= 0 {
		cfg.TunnelCheckInterval = DefaultTunnelCheckInterval
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Agent{
		cfg:  cfg,
		log:  log,
		wake: make(chan struct{}, 1),
	}, nil
}

// Run shares the line until ctx ends, then closes the connection and waits
// for every flow to finish. It reconnects with backoff whenever the
// connection fails. It returns nil once ctx ends, or an error if the agent
// is already running.
func (a *Agent) Run(ctx context.Context) error {
	if !a.running.CompareAndSwap(false, true) {
		return errors.New("agent: already running")
	}
	defer a.running.Store(false)
	defer a.setState(StateStopped)

	checkFileLimit(a.log)
	a.checkTunnel()
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { a.watchTunnel(ctx) })

	retry := newBackoff(a.cfg.ReconnectMin, a.cfg.ReconnectMax)
	for {
		if !a.waitActive(ctx) {
			return nil
		}
		res := a.runSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(res.err, errPaused) || !a.retrying(res.err) {
			// Paused, and maybe resumed already: not a failure, and
			// Resume reconnects right away.
			retry.reset()
			continue
		}
		if res.stable() {
			retry.reset()
		}
		delay := retry.next()
		if res.unauthorized() {
			delay = retry.longest()
		}
		a.log.Warn("gateway connection ended; reconnecting", "error", res.err, "retry_in", delay.Round(time.Millisecond))
		select {
		case <-time.After(delay):
		case <-a.wake:
		case <-ctx.Done():
			return nil
		}
	}
}

// Pause stops sharing: it closes the gateway connection, which ends every
// flow, and does not reconnect until Resume.
func (a *Agent) Pause() { a.setPaused(true) }

// Resume undoes Pause. Sharing restarts unless another VPN carries the
// default route.
func (a *Agent) Resume() { a.setPaused(false) }

// Stats returns a snapshot of the agent's state and counters.
func (a *Agent) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.past
	if a.cur != nil {
		v = v.add(a.cur.load())
	}
	return Stats{
		State:       a.state,
		LineID:      a.lineID,
		Paused:      a.paused,
		OtherVPN:    a.otherVPN,
		LastError:   a.lastErr,
		FlowsActive: v.flowsActive,
		FlowsTotal:  v.flowsTotal,
		BytesOut:    v.bytesOut,
		BytesIn:     v.bytesIn,
	}
}

func (a *Agent) setPaused(paused bool) {
	a.mu.Lock()
	changed := a.paused != paused
	a.paused = paused
	a.mu.Unlock()
	if !changed {
		return
	}
	if paused {
		a.log.Info("sharing paused")
	} else {
		a.log.Info("sharing resumed")
	}
	a.update()
}

func (a *Agent) setOtherVPN(on bool) {
	a.mu.Lock()
	changed := a.otherVPN != on
	a.otherVPN = on
	a.mu.Unlock()
	if !changed {
		return
	}
	if on {
		a.log.Warn("another VPN carries the default route; sharing paused")
	} else {
		a.log.Info("no other VPN carries the default route; sharing resumes")
	}
	a.update()
}

// update ends the current connection if sharing is no longer active, and
// nudges Run to act on the change.
func (a *Agent) update() {
	a.mu.Lock()
	if !a.activeLocked() {
		if a.endSession != nil {
			a.endSession(errPaused)
		}
		if a.state != StateStopped {
			a.state = StatePaused
		}
	}
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// retrying records why the last connection ended and reports whether the
// agent should reconnect, which it should unless sharing paused.
func (a *Agent) retrying(err error) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.activeLocked() {
		return false
	}
	a.lastErr = err
	a.state = StateConnecting
	return true
}

func (a *Agent) activeLocked() bool { return !a.paused && !a.otherVPN }

// waitActive blocks while sharing is paused. It reports false if ctx ended.
func (a *Agent) waitActive(ctx context.Context) bool {
	for {
		a.mu.Lock()
		if a.activeLocked() {
			a.state = StateConnecting
			a.mu.Unlock()
			return true
		}
		a.state = StatePaused
		a.mu.Unlock()
		select {
		case <-a.wake:
		case <-ctx.Done():
			return false
		}
	}
}

func (a *Agent) setState(s State) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = s
}

// checkTunnel runs TunnelCheck once. An error keeps the last answer and is
// logged once until a check succeeds again; offline, every check fails.
func (a *Agent) checkTunnel() {
	on, err := a.cfg.TunnelCheck()
	if err != nil {
		if !a.routeCheckFailing {
			a.log.Debug("route check failed", "error", err)
		}
		a.routeCheckFailing = true
		return
	}
	a.routeCheckFailing = false
	a.setOtherVPN(on)
}

func (a *Agent) watchTunnel(ctx context.Context) {
	t := time.NewTicker(a.cfg.TunnelCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.checkTunnel()
		case <-ctx.Done():
			return
		}
	}
}
