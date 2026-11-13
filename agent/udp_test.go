package agent

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

func TestUDPFramesRoundTripAsDatagrams(t *testing.T) {
	origin := startUDPEcho(t)
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(origin))
	s := g.nextConn(t).mustDial(t, relayprotocol.KindUDP, origin)

	datagrams := [][]byte{
		[]byte("first"),
		{}, // an empty datagram is a valid frame
		bytes.Repeat([]byte{0xab}, 1400),
		[]byte("last"),
	}
	_ = s.SetReadDeadline(time.Now().Add(waitFor))
	buf := make([]byte, relayprotocol.MaxUDPPayload)
	for i, d := range datagrams {
		// One at a time: the echo comes back over loopback UDP, which
		// does not promise order across datagrams in flight.
		if err := relayprotocol.WriteUDPFrame(s, d); err != nil {
			t.Fatal(err)
		}
		got, err := relayprotocol.ReadUDPFrame(s, buf)
		if err != nil {
			t.Fatalf("datagram %d: %v", i, err)
		}
		if !bytes.Equal(got, d) {
			t.Fatalf("datagram %d came back as %d bytes, want %d", i, len(got), len(d))
		}
	}
}

func TestUDPFlowEndsWhenTheGatewayEndsItsStreamBetweenFrames(t *testing.T) {
	origin := startUDPEcho(t)
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(origin))
	s := g.nextConn(t).mustDial(t, relayprotocol.KindUDP, origin)

	if err := relayprotocol.WriteUDPFrame(s, []byte("bye")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// The echo may or may not make it before the agent ends the flow, but
	// the stream must end cleanly between frames.
	_ = s.SetReadDeadline(time.Now().Add(waitFor))
	for {
		_, err := relayprotocol.ReadUDPFrame(s, nil)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("stream ended with %v, want a clean end", err)
		}
	}
}

func TestUDPStreamThatEndsInsideAFrameIsAProtocolError(t *testing.T) {
	origin := startUDPEcho(t)
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(origin))
	s := g.nextConn(t).mustDial(t, relayprotocol.KindUDP, origin)

	if _, err := s.Write([]byte{0x00, 0x10, 'x'}); err != nil { // claims 16 bytes, sends 1
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_ = s.SetReadDeadline(time.Now().Add(waitFor))
	_, err := io.ReadAll(s)
	var streamErr *quic.StreamError
	if !errors.As(err, &streamErr) || streamErr.ErrorCode != relayprotocol.CodeProtocolError.StreamErrorCode() {
		t.Fatalf("stream ended with %v, want PROTOCOL_ERROR", err)
	}
}

func TestIdleUDPFlowsEnd(t *testing.T) {
	restore := udpIdleTimeout
	udpIdleTimeout = 100 * time.Millisecond
	t.Cleanup(func() { udpIdleTimeout = restore })
	origin := startUDPEcho(t)
	g := newFakeGateway(t)
	startAgent(t, g, allowOrigins(origin))
	c := g.nextConn(t)

	s := c.mustDial(t, relayprotocol.KindUDP, origin)
	_ = s.SetReadDeadline(time.Now().Add(waitFor))
	_, err := io.ReadAll(s)
	var streamErr *quic.StreamError
	if !errors.As(err, &streamErr) || streamErr.ErrorCode != relayprotocol.CodeFlowCancelled.StreamErrorCode() {
		t.Fatalf("idle flow ended with %v, want FLOW_CANCELLED", err)
	}
	c.waitStats(t, func(st relayprotocol.Stats) bool { return st.FlowsTotal == 1 && st.FlowsActive == 0 })
}
