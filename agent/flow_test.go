package agent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

func TestMalformedFlowHeadersResetTheStream(t *testing.T) {
	g := newFakeGateway(t)
	startAgent(t, g, nil)
	c := g.nextConn(t)

	for _, tc := range []struct {
		name   string
		header []byte
		want   relayprotocol.Code
	}{
		{"unknown version", []byte{0x02, 0x01, 0x04, 0x00, 0x01, 0xbb, 11, 22, 33, 44}, relayprotocol.CodeUnsupportedVersion},
		{"reserved flag set", []byte{0x01, 0x01, 0x04, 0x80, 0x01, 0xbb, 11, 22, 33, 44}, relayprotocol.CodeProtocolError},
		{"unknown kind", []byte{0x01, 0x09, 0x04, 0x00, 0x01, 0xbb, 11, 22, 33, 44}, relayprotocol.CodeProtocolError},
		{"header cut short", []byte{0x01, 0x01, 0x04}, relayprotocol.CodeProtocolError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), waitFor)
			defer cancel()
			s, err := c.conn.OpenStreamSync(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Write(tc.header); err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
			_ = s.SetReadDeadline(time.Now().Add(waitFor))
			_, err = io.ReadAll(s)
			var streamErr *quic.StreamError
			if !errors.As(err, &streamErr) || streamErr.ErrorCode != tc.want.StreamErrorCode() {
				t.Fatalf("stream ended with %v, want %s", err, tc.want)
			}
		})
	}
}
