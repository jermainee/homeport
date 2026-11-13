package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// udpIdleTimeout ends a UDP flow that carried nothing in either direction
// for this long, so a flow the gateway forgot cannot hold a socket forever.
// Tests shorten it.
var udpIdleTimeout = 2 * time.Minute

// udpFrameBufSize fits the datagrams a WireGuard tunnel can carry. Larger
// frames still work; ReadUDPFrame allocates for them.
const udpFrameBufSize = 2048

var datagramBufs = sync.Pool{New: func() any {
	b := make([]byte, relayprotocol.MaxUDPPayload)
	return &b
}}

// relayUDP relays a UDP flow. The stream carries one UDP frame per datagram
// in each direction; the socket is connected to the destination.
func (a *Agent) relayUDP(ctx context.Context, f *flow, open relayprotocol.FlowOpen, pol *policy, cnt *counters) {
	c := a.dial(ctx, f, "udp", open.Dst, pol)
	if c == nil {
		return
	}
	closed := cnt.flowOpened()
	defer closed()

	r := &udpRelay{flow: f, conn: c.(*net.UDPConn), cnt: cnt}
	r.touch()
	stop := context.AfterFunc(ctx, func() { r.abort(relayprotocol.CodeFlowCancelled) })
	defer stop()
	var wg sync.WaitGroup
	wg.Go(r.up)
	r.down()
	wg.Wait()
	_ = r.conn.Close()
}

type udpRelay struct {
	flow *flow
	conn *net.UDPConn
	cnt  *counters

	lastActive atomic.Int64 // UnixNano of the last datagram either way
	ended      atomic.Bool  // the gateway ended its stream between frames
	once       sync.Once
}

func (r *udpRelay) touch() { r.lastActive.Store(time.Now().UnixNano()) }

func (r *udpRelay) idleSince() time.Time { return time.Unix(0, r.lastActive.Load()) }

func (r *udpRelay) abort(code relayprotocol.Code) {
	r.once.Do(func() {
		_ = r.conn.Close()
		r.flow.cancel(code)
	})
}

// up sends each of the gateway's frames as a datagram.
func (r *udpRelay) up() {
	buf := make([]byte, udpFrameBufSize)
	for {
		payload, err := relayprotocol.ReadUDPFrame(r.flow.stream, buf)
		if errors.Is(err, io.EOF) {
			// A stream that ends between frames ends the flow. down
			// sees the closed socket and closes the agent's side.
			r.ended.Store(true)
			_ = r.conn.Close()
			return
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			r.abort(relayprotocol.CodeProtocolError)
			return
		}
		if err != nil {
			r.abort(remoteCode(err))
			return
		}
		r.touch()
		// A failed send drops the datagram, as UDP would. It fails, for
		// example, after an ICMP error for an earlier datagram.
		if n, err := r.conn.Write(payload); err == nil {
			r.cnt.bytesOut.Add(uint64(n)) //nolint:gosec // n is never negative.
		}
	}
}

// down sends each datagram from the destination to the gateway as a frame.
// It is the only goroutine that writes to the stream.
func (r *udpRelay) down() {
	bp := datagramBufs.Get().(*[]byte)
	defer datagramBufs.Put(bp)
	buf := *bp
	for {
		_ = r.conn.SetReadDeadline(r.idleSince().Add(udpIdleTimeout))
		n, err := r.conn.Read(buf)
		if err != nil {
			var netErr net.Error
			switch {
			case r.ended.Load():
				r.flow.closeSend()
				return
			case errors.As(err, &netErr) && netErr.Timeout():
				if time.Since(r.idleSince()) >= udpIdleTimeout {
					r.abort(relayprotocol.CodeFlowCancelled)
					return
				}
			case isConnRefused(err), isConnReset(err):
				// An ICMP error for an earlier datagram; the flow goes on.
			default:
				r.abort(relayprotocol.CodeFlowCancelled)
				return
			}
			continue
		}
		r.touch()
		r.cnt.bytesIn.Add(uint64(n)) //nolint:gosec // n is never negative.
		if err := relayprotocol.WriteUDPFrame(r.flow.stream, buf[:n]); err != nil {
			r.abort(remoteCode(err))
			return
		}
	}
}
