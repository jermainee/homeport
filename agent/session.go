package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

const (
	// helloTimeout bounds the wait for the gateway's hello_ack.
	helloTimeout = 10 * time.Second
	// stableSession is how long a connection must last before a failure
	// counts as new, so the next retry starts at ReconnectMin again.
	stableSession = 30 * time.Second
)

// errPaused ends a connection because sharing paused.
var errPaused = errors.New("agent: sharing paused")

// errHomeChanged ends a connection because the home interface changed: its
// sockets are bound to the old one.
var errHomeChanged = errors.New("agent: home interface changed")

// sessionResult is how one connection attempt ended.
type sessionResult struct {
	err         error
	established bool          // the gateway acknowledged the hello
	lasted      time.Duration // from the hello_ack to the end
}

func (r sessionResult) stable() bool { return r.established && r.lasted >= stableSession }

// certificateRefused reports that the gateway refused the agent's
// certificate during the handshake. A gateway does that until its config
// lists the agent's line, as right after the member turned Share on; the
// refusal makes it fetch the config again.
func (r sessionResult) certificateRefused() bool { return relayprotocol.CertificateRefused(r.err) }

// unauthorized reports that the gateway rejected the agent, for example
// because its device was removed. Retrying soon will not help.
func (r sessionResult) unauthorized() bool {
	var appErr *quic.ApplicationError
	return errors.As(r.err, &appErr) && appErr.Remote &&
		appErr.ErrorCode == relayprotocol.CodeUnauthorized.ApplicationErrorCode()
}

// runSession dials the gateway, says hello and serves flows until the
// connection ends, ctx ends or sharing pauses. It returns once every flow of
// the connection has finished.
func (a *Agent) runSession(ctx context.Context) sessionResult {
	ctx, end := context.WithCancelCause(ctx)
	defer end(nil)
	home, ok := a.startSession(end)
	if !ok {
		return sessionResult{err: errPaused}
	}
	defer a.finishSession()
	// Every socket of the session is bound to the home interface it
	// started with; a new one ends the session.
	bind := func(c syscall.RawConn, network string, dst netip.Addr) error {
		return a.cfg.bind(c, network, dst, home)
	}

	a.log.Debug("dialing gateway", "gateway", a.cfg.GatewayAddr)
	conn, err := a.dialGateway(ctx, bind)
	if err != nil {
		return sessionResult{err: sessionError(ctx, fmt.Errorf("dial gateway: %w", err))}
	}
	// Close the connection when the session ends for any reason. Closing
	// it ends every flow stream, which unblocks their goroutines.
	stopClose := context.AfterFunc(ctx, func() {
		code, msg := closeCode(context.Cause(ctx))
		_ = conn.CloseWithError(code.ApplicationErrorCode(), msg)
	})
	defer stopClose()
	go func() {
		<-conn.Context().Done()
		end(context.Cause(conn.Context()))
	}()

	ctl, ack, err := hello(ctx, conn, a.cfg.Version)
	if err != nil {
		end(err)
		return sessionResult{err: sessionError(ctx, err)}
	}
	started := time.Now()
	cnt := a.connected(ack.LineID)
	a.log.Info("connected to gateway", "gateway", a.cfg.GatewayAddr, "line_id", ack.LineID)

	pol := newPolicy(ack.ObservedIP, a.cfg.testOrigins, a.cfg.testRoutes)
	pol.bind = bind
	var wg sync.WaitGroup
	wg.Go(func() { a.readControl(ctx, end, ctl) })
	wg.Go(func() { a.sendStats(ctx, end, ctl, cnt) })
	for {
		s, err := conn.AcceptStream(ctx)
		if err != nil {
			end(err)
			break
		}
		wg.Go(func() { a.serveFlow(ctx, s, pol, cnt) })
	}
	wg.Wait()

	res := sessionResult{err: sessionError(ctx, nil), established: true, lasted: time.Since(started)}
	total := cnt.load()
	a.log.Info("disconnected from gateway",
		"line_id", ack.LineID,
		"duration", res.lasted.Round(time.Millisecond),
		"flows", total.flowsTotal,
		"bytes_out", total.bytesOut,
		"bytes_in", total.bytesIn,
		"reason", res.err)
	return res
}

// dialGateway opens a QUIC connection to the gateway from a UDP socket of
// its own, bound with bind. The connection closes the socket when it ends.
func (a *Agent) dialGateway(ctx context.Context, bind func(syscall.RawConn, string, netip.Addr) error) (*quic.Conn, error) {
	host, port, err := net.SplitHostPort(a.cfg.GatewayAddr)
	if err != nil {
		return nil, err
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	// IPv4 first, as net.ResolveUDPAddr picks: the resolver orders the
	// addresses by the system's routes, which may be a VPN's, not by what
	// the home interface reaches.
	ip := addrs[0].Unmap()
	for _, addr := range addrs {
		if addr.Unmap().Is4() {
			ip = addr.Unmap()
			break
		}
	}
	portNum, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("gateway port %q: %w", port, err)
	}
	dst := netip.AddrPortFrom(ip, uint16(portNum))
	network := "udp4"
	if dst.Addr().Is6() {
		network = "udp6"
	}
	lc := net.ListenConfig{Control: func(network, _ string, c syscall.RawConn) error {
		return bind(c, network, dst.Addr())
	}}
	pc, err := lc.ListenPacket(ctx, network, ":0")
	if err != nil {
		return nil, err
	}
	tr := &quic.Transport{Conn: pc}
	tlsConf := relayprotocol.AgentTLSConfig(a.cfg.Certificate, relayprotocol.AllowPins(a.cfg.GatewayPin))
	conn, err := tr.Dial(ctx, net.UDPAddrFromAddrPort(dst), tlsConf, relayprotocol.AgentQUICConfig())
	if err != nil {
		_ = tr.Close()
		_ = pc.Close()
		return nil, err
	}
	context.AfterFunc(conn.Context(), func() {
		_ = tr.Close()
		_ = pc.Close()
	})
	return conn, nil
}

// sessionError is why the session ended: the cause of ctx if it ended,
// else err.
func sessionError(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

// closeCode picks the application error code the agent closes its
// connection with.
func closeCode(cause error) (relayprotocol.Code, string) {
	var proto *protocolError
	switch {
	case errors.As(cause, &proto):
		return relayprotocol.CodeProtocolError, ""
	case errors.Is(cause, errPaused):
		return relayprotocol.CodeGoingAway, "paused"
	case errors.Is(cause, errHomeChanged):
		return relayprotocol.CodeGoingAway, "network changed"
	case errors.Is(cause, context.Canceled):
		return relayprotocol.CodeGoingAway, "shutting down"
	default:
		return relayprotocol.CodeInternalError, ""
	}
}

// protocolError means the gateway broke the wire format on the control
// stream.
type protocolError struct{ err error }

func (e *protocolError) Error() string { return "gateway broke the control protocol: " + e.err.Error() }
func (e *protocolError) Unwrap() error { return e.err }

// hello opens the control stream, sends Hello and waits for HelloAck.
func hello(ctx context.Context, conn *quic.Conn, version string) (*quic.Stream, relayprotocol.HelloAck, error) {
	ctl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, relayprotocol.HelloAck{}, fmt.Errorf("open control stream: %w", err)
	}
	if err := relayprotocol.WriteControl(ctl, relayprotocol.Hello{AgentVersion: version, OS: runtime.GOOS}); err != nil {
		return nil, relayprotocol.HelloAck{}, fmt.Errorf("send hello: %w", err)
	}
	_ = ctl.SetReadDeadline(time.Now().Add(helloTimeout))
	defer func() { _ = ctl.SetReadDeadline(time.Time{}) }()
	for {
		msg, err := relayprotocol.ReadControl(ctl)
		if errors.Is(err, relayprotocol.ErrUnknownMessage) {
			continue
		}
		if err != nil {
			return nil, relayprotocol.HelloAck{}, fmt.Errorf("read hello_ack: %w", err)
		}
		ack, ok := msg.(relayprotocol.HelloAck)
		if !ok {
			return nil, relayprotocol.HelloAck{}, &protocolError{fmt.Errorf("got %T before hello_ack", msg)}
		}
		return ctl, ack, nil
	}
}

// readControl reads the gateway's control messages until the stream ends.
// None needs an answer yet; unknown ones are skipped.
func (a *Agent) readControl(ctx context.Context, end context.CancelCauseFunc, ctl *quic.Stream) {
	for {
		_, err := relayprotocol.ReadControl(ctl)
		if errors.Is(err, relayprotocol.ErrUnknownMessage) {
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				end(&protocolError{fmt.Errorf("control stream: %w", err)})
			}
			return
		}
	}
}

// sendStats sends the connection's counters every StatsInterval.
func (a *Agent) sendStats(ctx context.Context, end context.CancelCauseFunc, ctl *quic.Stream, cnt *counters) {
	t := time.NewTicker(a.cfg.StatsInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			v := cnt.load()
			msg := relayprotocol.Stats{
				FlowsActive: v.flowsActive,
				FlowsTotal:  v.flowsTotal,
				BytesOut:    v.bytesOut,
				BytesIn:     v.bytesIn,
			}
			if err := relayprotocol.WriteControl(ctl, msg); err != nil {
				end(fmt.Errorf("send stats: %w", err))
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// startSession registers end as the way to stop the coming connection. It
// reports false if sharing paused in the meantime.
func (a *Agent) startSession(end context.CancelCauseFunc) (Interface, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.activeLocked() {
		return Interface{}, false
	}
	a.endSession = end
	return a.home, true
}

// connected records the gateway's hello_ack and returns fresh counters for
// the connection.
func (a *Agent) connected(lineID string) *counters {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lineID = lineID
	a.lastErr = nil
	a.cur = &counters{}
	if a.activeLocked() {
		a.state = StateConnected
	}
	return a.cur
}

// finishSession folds the connection's counters into the totals. Every flow
// has finished by now, so the counters no longer change.
func (a *Agent) finishSession() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur != nil {
		a.past = a.past.add(a.cur.load())
		a.cur = nil
	}
	a.endSession = nil
	if a.state == StateConnected {
		a.state = StateConnecting
	}
}
