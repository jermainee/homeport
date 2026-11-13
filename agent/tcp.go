package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/quic-go/quic-go"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// relayTCP relays a TCP flow. The stream carries the raw byte stream in both
// directions. A FIN in either direction becomes a half-close on the other
// side, and a reset becomes a reset.
func (a *Agent) relayTCP(ctx context.Context, f *flow, open relayprotocol.FlowOpen, pol *policy, cnt *counters) {
	c := a.dial(ctx, f, "tcp", open.Dst.String(), pol)
	if c == nil {
		return
	}
	closed := cnt.flowOpened()
	defer closed()

	r := &tcpRelay{flow: f, conn: c.(*net.TCPConn), cnt: cnt}
	stop := context.AfterFunc(ctx, func() { r.abort(relayprotocol.CodeFlowCancelled) })
	defer stop()
	stopWatch := context.AfterFunc(f.stream.Context(), r.gatewayStoppedReading)
	defer stopWatch()
	var wg sync.WaitGroup
	wg.Go(r.up)
	r.down()
	wg.Wait()
	_ = r.conn.Close()
}

type tcpRelay struct {
	flow *flow
	conn *net.TCPConn
	cnt  *counters
	once sync.Once

	readStopped atomic.Bool // the gateway reads nothing more
}

// gatewayStoppedReading runs when the agent's side of the stream ends. If the
// gateway ended it (STOP_SENDING), the client is gone: a FLOW_RESET resets
// the socket, and anything else stops reading it, since nobody would read
// what the destination sends. The gateway's own bytes still reach the
// destination, up to its FIN.
func (r *tcpRelay) gatewayStoppedReading() {
	var streamErr *quic.StreamError
	if !errors.As(context.Cause(r.flow.stream.Context()), &streamErr) || !streamErr.Remote {
		return // the agent closed or cancelled it, or the connection ended
	}
	if streamErr.ErrorCode == relayprotocol.CodeFlowReset.StreamErrorCode() {
		r.abort(relayprotocol.CodeFlowReset)
		return
	}
	r.readStopped.Store(true)
	_ = r.conn.CloseRead()
}

// abort ends the flow in both directions. With FLOW_RESET the socket is
// reset (RST) instead of closed.
func (r *tcpRelay) abort(code relayprotocol.Code) {
	r.once.Do(func() {
		if code == relayprotocol.CodeFlowReset {
			_ = r.conn.SetLinger(0)
		}
		_ = r.conn.Close()
		r.flow.cancel(code)
	})
}

// up copies the gateway's bytes to the destination.
func (r *tcpRelay) up() {
	bp := relayBufs.Get().(*[]byte)
	defer relayBufs.Put(bp)
	buf := *bp
	for {
		n, err := r.flow.stream.Read(buf)
		if n > 0 {
			if _, werr := r.conn.Write(buf[:n]); werr != nil {
				r.abort(socketCode(werr))
				return
			}
			r.cnt.bytesOut.Add(uint64(n))
		}
		if errors.Is(err, io.EOF) {
			_ = r.conn.CloseWrite()
			return
		}
		if err != nil {
			r.abort(remoteCode(err))
			return
		}
	}
}

// down copies the destination's bytes to the gateway. It is the only
// goroutine that writes to the stream.
func (r *tcpRelay) down() {
	bp := relayBufs.Get().(*[]byte)
	defer relayBufs.Put(bp)
	buf := *bp
	for {
		n, err := r.conn.Read(buf)
		if r.readStopped.Load() {
			return
		}
		if n > 0 {
			r.cnt.bytesIn.Add(uint64(n))
			if _, werr := r.flow.stream.Write(buf[:n]); werr != nil {
				var streamErr *quic.StreamError
				if errors.As(werr, &streamErr) && streamErr.Remote &&
					streamErr.ErrorCode != relayprotocol.CodeFlowReset.StreamErrorCode() {
					return // the gateway stopped reading; gatewayStoppedReading takes over
				}
				r.abort(remoteCode(werr))
				return
			}
		}
		if errors.Is(err, io.EOF) {
			r.flow.closeSend()
			return
		}
		if err != nil {
			r.abort(socketCode(err))
			return
		}
	}
}

// socketCode is the code to end a flow with after a socket error: FLOW_RESET
// if the destination reset the connection, FLOW_CANCELLED otherwise.
func socketCode(err error) relayprotocol.Code {
	if isConnReset(err) {
		return relayprotocol.CodeFlowReset
	}
	return relayprotocol.CodeFlowCancelled
}
