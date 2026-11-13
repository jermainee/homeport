package wgstack

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"time"
)

// maxHello bounds what peekServerName reads: a ClientHello, even with large
// post-quantum key shares, fits well within it.
const maxHello = 64 << 10

var errHelloRead = errors.New("wgstack: ClientHello read")

// peekServerName reads a TLS ClientHello from conn and returns the server
// name it names (SNI) and every byte read, which must be passed on before
// anything else. Nothing is written to conn.
func peekServerName(conn net.Conn, deadline time.Time) (string, []byte, error) {
	if err := conn.SetReadDeadline(deadline); err != nil {
		return "", nil, err
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	rec := &recordingConn{Conn: conn}
	var name string
	srv := tls.Server(rec, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			name = hello.ServerName
			return nil, errHelloRead // stop: the handshake goes on with the real server
		},
	})
	err := srv.Handshake()
	if name == "" || !errors.Is(err, errHelloRead) {
		if err == nil || errors.Is(err, errHelloRead) {
			err = errors.New("wgstack: ClientHello names no server")
		}
		return "", nil, err
	}
	return name, rec.buf.Bytes(), nil
}

// recordingConn keeps every byte read through it and drops every write,
// such as the alert the TLS server would send after stopping.
type recordingConn struct {
	net.Conn
	buf bytes.Buffer
}

func (c *recordingConn) Read(b []byte) (int, error) {
	if c.buf.Len() >= maxHello {
		return 0, io.ErrUnexpectedEOF
	}
	if len(b) > maxHello-c.buf.Len() {
		b = b[:maxHello-c.buf.Len()]
	}
	n, err := c.Conn.Read(b)
	c.buf.Write(b[:n])
	return n, err
}

func (c *recordingConn) Write(b []byte) (int, error) { return len(b), nil }
