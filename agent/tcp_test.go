package agent

import (
	"bytes"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

func TestTCPFlowKeepsTheReplyAfterTheClientHalfCloses(t *testing.T) {
	// The origin reads the request to its end and only then answers, so
	// the answer arrives only if the agent passed the FIN on as a
	// half-close instead of closing the socket.
	origin := startTCPOrigin(t, func(c *net.TCPConn) {
		req, err := io.ReadAll(c)
		if err != nil {
			return
		}
		_, _ = c.Write(append([]byte("got: "), req...))
	})
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(origin))

	s := g.nextConn(t).mustDial(t, relayprotocol.KindTCP, origin)
	if _, err := s.Write([]byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := string(readAll(t, s)), "got: GET / HTTP/1.0\r\n\r\n"; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
}

func TestTCPFlowKeepsSendingAfterTheDestinationHalfCloses(t *testing.T) {
	received := make(chan string, 1)
	origin := startTCPOrigin(t, func(c *net.TCPConn) {
		_, _ = c.Write([]byte("banner"))
		_ = c.CloseWrite()
		b, _ := io.ReadAll(c)
		received <- string(b)
	})
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(origin))

	s := g.nextConn(t).mustDial(t, relayprotocol.KindTCP, origin)
	if got := string(readAll(t, s)); got != "banner" {
		t.Fatalf("read %q, want the banner and then the end of the stream", got)
	}
	if _, err := s.Write([]byte("still talking")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "still talking" {
			t.Fatalf("origin received %q, want %q", got, "still talking")
		}
	case <-time.After(waitFor):
		t.Fatal("origin never saw the end of the stream")
	}
}

func TestTCPFlowResetByTheClientResetsTheDestination(t *testing.T) {
	readErr := make(chan error, 1)
	origin := startTCPOrigin(t, func(c *net.TCPConn) {
		_, err := io.ReadAll(c)
		readErr <- err
	})
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(origin))

	s := g.nextConn(t).mustDial(t, relayprotocol.KindTCP, origin)
	if _, err := s.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	s.CancelWrite(relayprotocol.CodeFlowReset.StreamErrorCode())
	s.CancelRead(relayprotocol.CodeFlowReset.StreamErrorCode())

	select {
	case err := <-readErr:
		if !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("origin read ended with %v, want a connection reset", err)
		}
	case <-time.After(waitFor):
		t.Fatal("origin connection stayed open")
	}
}

func TestTCPFlowResetByTheDestinationResetsTheStream(t *testing.T) {
	origin := startTCPOrigin(t, func(c *net.TCPConn) {
		_ = c.SetLinger(0) // Close sends RST.
	})
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(origin))

	// The reset can overtake the OK result: a QUIC stream reset does not
	// deliver data still in flight. Either way the stream ends in
	// FLOW_RESET, and anything that arrived first is the OK result.
	s := g.nextConn(t).openFlow(t, relayprotocol.KindTCP, origin)
	_ = s.SetReadDeadline(time.Now().Add(waitFor))
	got, err := io.ReadAll(s)
	var streamErr *quic.StreamError
	if !errors.As(err, &streamErr) || streamErr.ErrorCode != relayprotocol.CodeFlowReset.StreamErrorCode() {
		t.Fatalf("stream read ended with %v, want FLOW_RESET", err)
	}
	if len(got) != 0 && !bytes.Equal(got, []byte{relayprotocol.Version, byte(relayprotocol.StatusOK)}) {
		t.Fatalf("read %x before the reset, want nothing or the OK result", got)
	}
}

func TestTCPFlowEndsWhenTheGatewayClosesItEvenIfTheDestinationStaysSilent(t *testing.T) {
	// The origin keeps its socket open and never writes, so only the
	// gateway's close can end the flow.
	received := make(chan string, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	origin := startTCPOrigin(t, func(c *net.TCPConn) {
		b, _ := io.ReadAll(c)
		received <- string(b)
		<-release
	})
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(origin))
	c := g.nextConn(t)

	s := c.mustDial(t, relayprotocol.KindTCP, origin)
	if _, err := s.Write([]byte("QUIT\r\n")); err != nil {
		t.Fatal(err)
	}
	// A full close, as the gateway does when the client is gone.
	s.CancelRead(relayprotocol.CodeFlowCancelled.StreamErrorCode())
	_ = s.Close()

	select {
	case got := <-received:
		if got != "QUIT\r\n" {
			t.Fatalf("origin received %q, want the bytes sent before the close", got)
		}
	case <-time.After(waitFor):
		t.Fatal("origin never saw the end of the stream")
	}
	c.waitStats(t, func(st relayprotocol.Stats) bool { return st.FlowsTotal == 1 && st.FlowsActive == 0 })
}

func TestTCPFlowToAClosedPortIsRefused(t *testing.T) {
	dst := closedPort(t)
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(dst))

	s, status := g.nextConn(t).dial(t, relayprotocol.KindTCP, dst)
	if status != relayprotocol.StatusRefused {
		t.Fatalf("status = %s, want REFUSED", status)
	}
	if rest := readAll(t, s); len(rest) != 0 {
		t.Fatalf("agent sent %q after a refusal, want the end of the stream", rest)
	}
}
