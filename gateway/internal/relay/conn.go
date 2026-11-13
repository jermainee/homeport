package relay

import (
	"net"
	"time"

	"github.com/quic-go/quic-go"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// streamConn is a flow stream used as a byte stream (TCP and DNS over TCP).
type streamConn struct {
	stream *quic.Stream
	conn   *quic.Conn
}

func (c *streamConn) Read(b []byte) (int, error)  { return c.stream.Read(b) }
func (c *streamConn) Write(b []byte) (int, error) { return c.stream.Write(b) }

// CloseWrite passes a FIN on: it closes the send side of the stream.
func (c *streamConn) CloseWrite() error { return c.stream.Close() }

// Close ends the flow in both directions.
func (c *streamConn) Close() error {
	c.stream.CancelRead(relayprotocol.CodeFlowCancelled.StreamErrorCode())
	return c.stream.Close()
}

func (c *streamConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *streamConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *streamConn) SetDeadline(t time.Time) error      { return c.stream.SetDeadline(t) }
func (c *streamConn) SetReadDeadline(t time.Time) error  { return c.stream.SetReadDeadline(t) }
func (c *streamConn) SetWriteDeadline(t time.Time) error { return c.stream.SetWriteDeadline(t) }

// frameConn is a flow stream that carries datagrams as UDP frames (UDP and
// DNS over UDP). Each Read returns one datagram and each Write sends one.
type frameConn struct {
	streamConn
}

func (c *frameConn) Read(b []byte) (int, error) {
	payload, err := relayprotocol.ReadUDPFrame(c.stream, b)
	if err != nil {
		return 0, err
	}
	// A datagram larger than b is truncated, as a UDP socket would.
	return copy(b, payload), nil
}

func (c *frameConn) Write(b []byte) (int, error) {
	if err := relayprotocol.WriteUDPFrame(c.stream, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// A datagram flow has no half-close; hide streamConn's CloseWrite.
func (c *frameConn) CloseWrite() error { return c.Close() }
