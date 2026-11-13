package agent

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

const (
	// headerTimeout bounds the wait for a FlowOpen header on a new stream.
	headerTimeout = 10 * time.Second
	// dialTimeout bounds opening a socket to a destination.
	dialTimeout = 10 * time.Second
)

// serveFlow answers one flow stream the gateway opened: it reads the
// FlowOpen header, opens a socket on the member's line if the policy allows
// it, answers with a FlowResult and relays until the flow ends. Its logs hold
// the flow's kind, status and byte counts, never its destination.
func (a *Agent) serveFlow(ctx context.Context, s *quic.Stream, pol *policy, cnt *counters) {
	_ = s.SetReadDeadline(time.Now().Add(headerTimeout))
	open, err := relayprotocol.ReadFlowOpen(s)
	_ = s.SetReadDeadline(time.Time{})
	if err != nil {
		code := relayprotocol.CodeProtocolError
		if errors.Is(err, relayprotocol.ErrUnsupportedVersion) {
			code = relayprotocol.CodeUnsupportedVersion
		}
		s.CancelRead(code.StreamErrorCode())
		s.CancelWrite(code.StreamErrorCode())
		// relay-protocol's errors never contain a destination.
		a.log.Debug("flow header rejected", "error", err)
		return
	}

	f := &flow{stream: s, kind: open.Kind}
	switch open.Kind {
	case relayprotocol.KindTCP:
		a.relayTCP(ctx, f, open, pol, cnt)
	case relayprotocol.KindUDP:
		a.relayUDP(ctx, f, open, pol, cnt)
	case relayprotocol.KindDNS:
		a.serveDNS(ctx, f, pol, cnt)
	default:
		f.cancel(relayprotocol.CodeInternalError)
	}
}

// flow is a flow stream with the bookkeeping that quic-go's rules need:
// Close must not follow CancelWrite, and must not run concurrently with
// Write. Only the goroutine that writes to the stream calls closeSend.
type flow struct {
	stream *quic.Stream
	kind   relayprotocol.Kind

	mu            sync.Mutex
	sendCancelled bool
}

// closeSend ends the agent's side of the stream with a FIN, unless the flow
// was cancelled.
func (f *flow) closeSend() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.sendCancelled {
		_ = f.stream.Close()
	}
}

// cancel aborts both directions of the stream with code.
func (f *flow) cancel(code relayprotocol.Code) {
	f.stream.CancelRead(code.StreamErrorCode())
	f.mu.Lock()
	f.sendCancelled = true
	f.mu.Unlock()
	f.stream.CancelWrite(code.StreamErrorCode())
}

// answer writes the FlowResult. A flow that is not OK ends here: the agent
// closes its side and discards whatever payload the gateway sent. It reports
// whether the flow goes on.
func (a *Agent) answer(f *flow, status relayprotocol.Status, rule string) bool {
	if err := relayprotocol.WriteFlowResult(f.stream, relayprotocol.FlowResult{Status: status}); err != nil {
		f.cancel(relayprotocol.CodeFlowCancelled)
		return false
	}
	if status == relayprotocol.StatusOK {
		return true
	}
	f.closeSend()
	f.stream.CancelRead(relayprotocol.CodeFlowCancelled.StreamErrorCode())
	if rule != "" {
		a.log.Debug("flow refused by policy", "kind", f.kind, "rule", rule)
	} else {
		a.log.Debug("flow not opened", "kind", f.kind, "status", status)
	}
	return false
}

// dial opens a socket to dst through the policy's dialer and answers the
// flow. It returns nil if the flow ended with its answer.
func (a *Agent) dial(ctx context.Context, f *flow, network string, dst string, pol *policy) net.Conn {
	c, err := pol.dialer().DialContext(ctx, network, dst)
	if err != nil {
		// err names the destination, so only its status is kept.
		status, rule := dialStatus(err)
		a.answer(f, status, rule)
		return nil
	}
	if !a.answer(f, relayprotocol.StatusOK, "") {
		_ = c.Close()
		return nil
	}
	return c
}

// dialStatus maps a dial error to the FlowResult status the gateway gets, and
// to the policy rule if the policy refused the destination.
func dialStatus(err error) (relayprotocol.Status, string) {
	var (
		polErr *policyError
		netErr net.Error
	)
	switch {
	case errors.As(err, &polErr):
		return relayprotocol.StatusPolicy, polErr.rule
	case isConnRefused(err):
		return relayprotocol.StatusRefused, ""
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return relayprotocol.StatusTimeout, ""
	default:
		return relayprotocol.StatusUnreachable, ""
	}
}

// gatewayCancelled reports whether err is the gateway cancelling the stream
// (RESET_STREAM or STOP_SENDING), and whether it did so with FLOW_RESET, the
// code for a TCP reset.
func gatewayCancelled(err error) (cancelled, reset bool) {
	var streamErr *quic.StreamError
	if !errors.As(err, &streamErr) || !streamErr.Remote {
		return false, false
	}
	return true, streamErr.ErrorCode == relayprotocol.CodeFlowReset.StreamErrorCode()
}

// remoteCode is the code to end a flow with after a stream error: FLOW_RESET
// if the gateway reset the flow as a TCP reset, FLOW_CANCELLED otherwise.
func remoteCode(err error) relayprotocol.Code {
	if _, reset := gatewayCancelled(err); reset {
		return relayprotocol.CodeFlowReset
	}
	return relayprotocol.CodeFlowCancelled
}

// relayBufSize is the copy buffer of a TCP flow direction.
const relayBufSize = 32 << 10

var relayBufs = sync.Pool{New: func() any {
	b := make([]byte, relayBufSize)
	return &b
}}
