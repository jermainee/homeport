// Package agent is Homeport's sharing agent. It runs on a member's home
// machine, dials out to an edge gateway over QUIC and opens a real socket on
// the member's line for every flow the gateway relays to it.
//
// The wire format is relay-protocol (see its README). The agent refuses flows
// into the member's home network and to mail ports (see policy.go). It binds
// every socket it opens to the home line's interface, so shared traffic
// leaves through the member's line even while a VPN carries the machine's
// default route, and pauses only while no interface but a VPN's reaches the
// internet. It logs session metadata only: never a destination, a DNS name or
// content.
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
	"syscall"
	"time"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"

	"github.com/jermainee/homeport/agent/internal/netwatch"
)

// Version is the agent release reported to the gateway when Config.Version
// is empty.
const Version = "0.1.0"

// Defaults for the zero values in Config.
const (
	DefaultStatsInterval      = 30 * time.Second
	DefaultReconnectMin       = time.Second
	DefaultReconnectMax       = time.Minute
	DefaultRouteCheckInterval = time.Second
)

// certificateRetries is how many times in a row the agent retries after
// ReconnectMin when the gateway refuses its certificate. A gateway that does
// not list the agent's line yet fetches its config at once, or at most 2 s
// later, so five retries (2.5 to 5 s with the default ReconnectMin) land
// soon after it. Then the usual backoff takes over.
const certificateRetries = 5

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

	// Resolver answers DNS flows. Nil means DNS over HTTPS to Cloudflare,
	// then Quad9, through the line, verified against the roots built into
	// the agent, so neither the member's network nor their ISP can read or
	// change the answers. Tests pass their own.
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

	// RouteCheck reports how the machine reaches the internet; prev is the
	// home interface it reported last. The agent binds every socket it
	// opens, to the gateway, to destinations and to its DNS resolvers, to
	// the home interface, so shared traffic leaves through the member's own
	// line even while a VPN, such as their own Use tunnel, carries the
	// default route. Sharing pauses while only a VPN reaches the internet,
	// and reconnects when the home interface changes. Nil means the
	// system's check on macOS and Linux; elsewhere it reports nothing, and
	// sockets follow the system's routes.
	RouteCheck func(prev string) (Route, error)
	// RouteCheckInterval is how often RouteCheck runs. Zero means
	// DefaultRouteCheckInterval.
	RouteCheckInterval time.Duration

	// bind ties a socket to the home interface before it connects. Tests
	// record the calls instead; nil means netwatch.Bind.
	bind func(c syscall.RawConn, network string, dst netip.Addr, home Interface) error

	// testOrigins are loopback addresses the policy lets through, and
	// testRoutes send a flow's destination to one of them instead. Only
	// this package's tests and e2e builds (policy_testallow.go) set them.
	testOrigins []netip.AddrPort
	testRoutes  map[netip.AddrPort]netip.AddrPort
}

// Route is how the machine reaches the internet: its home interface, and
// whether a VPN carries the default route.
type Route = netwatch.Route

// Interface names a network interface by index and name.
type Interface = netwatch.Interface

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
	// StatePaused means sharing is paused, by Pause or because only
	// another VPN reaches the internet. The agent holds no connection.
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
	// OtherVPN reports that sharing paused because only another VPN
	// reaches the internet: no interface but the VPN's has a default
	// route, as with a VPN's kill switch.
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
	wake    chan struct{} // nudges Run after Pause, Resume or a route change

	routeCheckFailing bool // only the goroutine running checkRoute uses it

	mu         sync.Mutex
	state      State
	lineID     string
	paused     bool
	otherVPN   bool
	home       Interface // the interface new sockets are bound to
	lastHome   string    // the last home's name, kept while only a VPN reaches the internet
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
		origins[i] = unmapPort(o)
	}
	cfg.testOrigins = origins
	routes := make(map[netip.AddrPort]netip.AddrPort, len(cfg.testRoutes))
	for dst, o := range cfg.testRoutes {
		routes[unmapPort(dst)] = unmapPort(o) // o is in testOrigins too
	}
	cfg.testRoutes = routes
	if cfg.Version == "" {
		cfg.Version = Version
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
	if cfg.RouteCheck == nil {
		cfg.RouteCheck = netwatch.Check
	}
	if cfg.RouteCheckInterval <= 0 {
		cfg.RouteCheckInterval = DefaultRouteCheckInterval
	}
	if cfg.bind == nil {
		cfg.bind = netwatch.Bind
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	a := &Agent{log: log, wake: make(chan struct{}, 1)}
	if cfg.Resolver == nil {
		r := newDoHResolver(defaultDoHServers, bundledRoots())
		r.log = log
		cfg.Resolver = r
	}
	if r, ok := cfg.Resolver.(*dohResolver); ok {
		// Lookups go through the line, as the member's own flows do.
		r.bind = a.bindHome
	}
	a.cfg = cfg
	return a, nil
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
	if c, ok := a.cfg.Resolver.(interface{ CloseIdleConnections() }); ok {
		defer c.CloseIdleConnections() // after every flow has finished
	}

	checkFileLimit(a.log)
	a.checkRoute()
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { a.watchRoute(ctx) })

	retry := newBackoff(a.cfg.ReconnectMin, a.cfg.ReconnectMax)
	refused := 0 // certificate refusals in a row
	for {
		if !a.waitActive(ctx) {
			return nil
		}
		res := a.runSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if res.certificateRefused() {
			refused++
		} else {
			refused = 0
		}
		if errors.Is(res.err, errHomeChanged) {
			// The session's lookups have ended, so the resolver's
			// connections from the old interface are idle now.
			a.closeResolverIdle()
		}
		if errors.Is(res.err, errPaused) || errors.Is(res.err, errHomeChanged) || !a.retrying(res.err) {
			// Paused, and maybe resumed already, or moved to another
			// network: not a failure, and the agent reconnects right
			// away once sharing is active.
			retry.reset()
			continue
		}
		if res.stable() {
			retry.reset()
		}
		var delay time.Duration
		if refused > 0 && refused <= certificateRetries {
			// The gateway may not know the agent's line yet, and is
			// fetching its config because of this attempt.
			delay = retry.shortest()
		} else {
			delay = retry.next()
			if res.unauthorized() {
				delay = retry.longest()
			}
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

// Resume undoes Pause. Sharing restarts unless only another VPN reaches the
// internet.
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

// setRoute records a route check's answer. Sharing pauses while only a VPN
// reaches the internet. A new home interface ends the connection, whose
// sockets are bound to the old one, so the agent reconnects through the new
// one; the resolver's idle connections close for the same reason, and Run
// closes the rest once the connection's lookups have ended.
func (a *Agent) setRoute(r Route) {
	vpnOnly := r.Tunneled && r.Home.Index == 0
	a.mu.Lock()
	vpnChanged := a.otherVPN != vpnOnly
	homeChanged := a.home != r.Home
	first := a.lastHome == ""
	old := a.home
	a.otherVPN = vpnOnly
	a.home = r.Home
	if r.Home.Name != "" {
		a.lastHome = r.Home.Name
	}
	end := a.endSession
	a.mu.Unlock()
	if vpnChanged {
		if vpnOnly {
			a.log.Warn("only another VPN reaches the internet; sharing paused")
		} else {
			a.log.Info("the home line reaches the internet again; sharing resumes")
		}
	}
	switch {
	case homeChanged && first:
		a.log.Info("home interface", "interface", r.Home.Name, "vpn", r.Tunneled)
	case homeChanged:
		a.log.Info("home interface changed", "from", old.Name, "to", r.Home.Name, "vpn", r.Tunneled)
		if end != nil && !vpnOnly {
			end(errHomeChanged)
		}
		a.closeResolverIdle()
	}
	if vpnChanged || homeChanged {
		a.update()
	}
}

// closeResolverIdle closes the resolver's idle connections, which are bound
// to the home interface they were opened through.
func (a *Agent) closeResolverIdle() {
	if c, ok := a.cfg.Resolver.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// homeNow returns the interface new sockets are bound to.
func (a *Agent) homeNow() Interface {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.home
}

// bindHome binds a socket to the current home interface.
func (a *Agent) bindHome(c syscall.RawConn, network string, dst netip.Addr) error {
	return a.cfg.bind(c, network, dst, a.homeNow())
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

// checkRoute runs RouteCheck once. An error keeps the last answer and is
// logged once until a check succeeds again; offline, every check fails.
func (a *Agent) checkRoute() {
	a.mu.Lock()
	prev := a.lastHome // the home before a pause for a VPN, too
	a.mu.Unlock()
	r, err := a.cfg.RouteCheck(prev)
	if err != nil {
		if !a.routeCheckFailing {
			a.log.Debug("route check failed", "error", err)
		}
		a.routeCheckFailing = true
		return
	}
	a.routeCheckFailing = false
	a.setRoute(r)
}

func (a *Agent) watchRoute(ctx context.Context) {
	t := time.NewTicker(a.cfg.RouteCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.checkRoute()
		case <-ctx.Done():
			return
		}
	}
}
