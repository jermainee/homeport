package relayprotocol_test

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"

	rp "github.com/jermainee/homeport/relay-protocol"
)

func TestFlowOpenWireFormat(t *testing.T) {
	tests := []struct {
		name string
		open rp.FlowOpen
		wire []byte
	}{
		{
			name: "TCP to IPv4",
			open: rp.FlowOpen{Kind: rp.KindTCP, Dst: netip.MustParseAddrPort("203.0.113.7:443")},
			wire: []byte{0x01, 0x01, 0x04, 0x00, 0x01, 0xbb, 203, 0, 113, 7},
		},
		{
			name: "UDP to IPv6",
			open: rp.FlowOpen{Kind: rp.KindUDP, Dst: netip.MustParseAddrPort("[2001:db8::1]:3478")},
			wire: []byte{
				0x01, 0x02, 0x06, 0x00, 0x0d, 0x96,
				0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01,
			},
		},
		{
			name: "DNS to the virtual resolver",
			open: rp.FlowOpen{Kind: rp.KindDNS, Dst: netip.MustParseAddrPort("10.77.0.1:53")},
			wire: []byte{0x01, 0x03, 0x04, 0x00, 0x00, 0x35, 10, 77, 0, 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := rp.WriteFlowOpen(&buf, tt.open); err != nil {
				t.Fatalf("WriteFlowOpen: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), tt.wire) {
				t.Fatalf("wire = % x, want % x", buf.Bytes(), tt.wire)
			}
			got, err := rp.ReadFlowOpen(bytes.NewReader(tt.wire))
			if err != nil {
				t.Fatalf("ReadFlowOpen: %v", err)
			}
			if got != tt.open {
				t.Fatalf("ReadFlowOpen = %+v, want %+v", got, tt.open)
			}
		})
	}
}

func TestFlowOpenRoundTripsEveryKindAndFamily(t *testing.T) {
	kinds := []rp.Kind{rp.KindTCP, rp.KindUDP, rp.KindDNS}
	dsts := []netip.AddrPort{
		netip.MustParseAddrPort("198.51.100.1:1"),
		netip.MustParseAddrPort("255.255.255.255:65535"),
		netip.MustParseAddrPort("[2001:db8::]:8443"),
		netip.MustParseAddrPort("[ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]:65535"),
	}
	for _, kind := range kinds {
		for _, dst := range dsts {
			open := rp.FlowOpen{Kind: kind, Dst: dst}
			var buf bytes.Buffer
			if err := rp.WriteFlowOpen(&buf, open); err != nil {
				t.Fatalf("WriteFlowOpen(%+v): %v", open, err)
			}
			got, err := rp.ReadFlowOpen(&buf)
			if err != nil {
				t.Fatalf("ReadFlowOpen(%+v): %v", open, err)
			}
			if got != open {
				t.Fatalf("round trip = %+v, want %+v", got, open)
			}
		}
	}
}

func TestReadFlowOpenLeavesPipelinedPayloadUnread(t *testing.T) {
	header := []byte{0x01, 0x01, 0x04, 0x00, 0x00, 0x50, 192, 0, 2, 10}
	payload := []byte("GET / HTTP/1.1\r\n")
	r := bytes.NewReader(append(header, payload...))

	if _, err := rp.ReadFlowOpen(r); err != nil {
		t.Fatalf("ReadFlowOpen: %v", err)
	}
	rest, _ := io.ReadAll(r)
	if !bytes.Equal(rest, payload) {
		t.Fatalf("bytes after header = %q, want %q", rest, payload)
	}
}

func TestReadFlowOpenRejectsMalformedHeaders(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
		want error
	}{
		{"empty stream", nil, io.EOF},
		{"unknown version", []byte{0x02, 0x01, 0x04, 0x00, 0x01, 0xbb, 1, 2, 3, 4}, rp.ErrUnsupportedVersion},
		{"version zero", []byte{0x00, 0x01, 0x04, 0x00, 0x01, 0xbb, 1, 2, 3, 4}, rp.ErrUnsupportedVersion},
		{"kind zero", []byte{0x01, 0x00, 0x04, 0x00, 0x01, 0xbb, 1, 2, 3, 4}, rp.ErrMalformed},
		{"unknown kind", []byte{0x01, 0x04, 0x04, 0x00, 0x01, 0xbb, 1, 2, 3, 4}, rp.ErrMalformed},
		{"unknown family", []byte{0x01, 0x01, 0x05, 0x00, 0x01, 0xbb, 1, 2, 3, 4}, rp.ErrMalformed},
		{"reserved flag set", []byte{0x01, 0x01, 0x04, 0x01, 0x01, 0xbb, 1, 2, 3, 4}, rp.ErrMalformed},
		{"port zero", []byte{0x01, 0x01, 0x04, 0x00, 0x00, 0x00, 1, 2, 3, 4}, rp.ErrMalformed},
		{
			"IPv4-mapped address sent as IPv6",
			[]byte{0x01, 0x01, 0x06, 0x00, 0x01, 0xbb, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 10, 0, 0, 1},
			rp.ErrMalformed,
		},
		{"truncated before port", []byte{0x01, 0x01, 0x04, 0x00}, io.ErrUnexpectedEOF},
		{"truncated IPv4 address", []byte{0x01, 0x01, 0x04, 0x00, 0x01, 0xbb, 1, 2, 3}, io.ErrUnexpectedEOF},
		{"IPv6 family with IPv4-sized address", []byte{0x01, 0x01, 0x06, 0x00, 0x01, 0xbb, 1, 2, 3, 4}, io.ErrUnexpectedEOF},
		{"header without address", []byte{0x01, 0x01, 0x04, 0x00, 0x01, 0xbb}, io.ErrUnexpectedEOF},
		{"version byte only", []byte{0x01}, io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rp.ReadFlowOpen(bytes.NewReader(tt.wire))
			if !errors.Is(err, tt.want) {
				t.Fatalf("ReadFlowOpen error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestReadFlowOpenStopsAtUnknownVersion(t *testing.T) {
	// A future version may use a different header length, so nothing past
	// the version byte may be consumed.
	r := bytes.NewReader([]byte{0x02, 0xaa, 0xbb})
	if _, err := rp.ReadFlowOpen(r); !errors.Is(err, rp.ErrUnsupportedVersion) {
		t.Fatalf("ReadFlowOpen error = %v, want ErrUnsupportedVersion", err)
	}
	if r.Len() != 2 {
		t.Fatalf("ReadFlowOpen consumed %d bytes past the version byte", 2-r.Len())
	}
}

func TestWriteFlowOpenRejectsInvalidFlows(t *testing.T) {
	tests := []struct {
		name string
		open rp.FlowOpen
	}{
		{"kind zero", rp.FlowOpen{Dst: netip.MustParseAddrPort("192.0.2.1:443")}},
		{"unknown kind", rp.FlowOpen{Kind: 9, Dst: netip.MustParseAddrPort("192.0.2.1:443")}},
		{"no address", rp.FlowOpen{Kind: rp.KindTCP}},
		{"port zero", rp.FlowOpen{Kind: rp.KindTCP, Dst: netip.MustParseAddrPort("192.0.2.1:0")}},
		{"IPv4-mapped IPv6", rp.FlowOpen{Kind: rp.KindTCP, Dst: netip.MustParseAddrPort("[::ffff:10.0.0.1]:443")}},
		{"zoned IPv6", rp.FlowOpen{Kind: rp.KindTCP, Dst: netip.MustParseAddrPort("[fe80::1%en0]:443")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := rp.WriteFlowOpen(&buf, tt.open)
			if !errors.Is(err, rp.ErrMalformed) {
				t.Fatalf("WriteFlowOpen error = %v, want ErrMalformed", err)
			}
			if buf.Len() != 0 {
				t.Fatalf("WriteFlowOpen wrote % x for an invalid flow", buf.Bytes())
			}
		})
	}
}

func TestFlowOpenWrittenInOneWrite(t *testing.T) {
	// A QUIC stream may send each Write in its own packet, so the header
	// must not be split across writes.
	w := &countingWriter{}
	open := rp.FlowOpen{Kind: rp.KindTCP, Dst: netip.MustParseAddrPort("[2001:db8::7]:443")}
	if err := rp.WriteFlowOpen(w, open); err != nil {
		t.Fatalf("WriteFlowOpen: %v", err)
	}
	if w.writes != 1 {
		t.Fatalf("WriteFlowOpen used %d writes, want 1", w.writes)
	}
}

type countingWriter struct {
	writes int
	bytes.Buffer
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

func FuzzReadFlowOpen(f *testing.F) {
	f.Add([]byte{0x01, 0x01, 0x04, 0x00, 0x01, 0xbb, 203, 0, 113, 7})
	f.Add([]byte{0x01, 0x02, 0x06, 0x00, 0x0d, 0x96, 0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01})
	f.Add([]byte{0x01, 0x03, 0x04, 0x00, 0x00, 0x35, 10, 77, 0, 1, 0xde, 0xad})
	f.Add([]byte{0x01, 0x01, 0x06, 0x00, 0x01, 0xbb, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 10, 0, 0, 1})
	f.Add([]byte{0x02})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		open, err := rp.ReadFlowOpen(r)
		if err != nil {
			return
		}
		consumed := data[:len(data)-r.Len()]
		var buf bytes.Buffer
		if err := rp.WriteFlowOpen(&buf, open); err != nil {
			t.Fatalf("decoded %+v from % x but cannot encode it: %v", open, consumed, err)
		}
		if !bytes.Equal(buf.Bytes(), consumed) {
			t.Fatalf("decoded % x as %+v, which encodes as % x", consumed, open, buf.Bytes())
		}
	})
}

func TestFlowResultWireFormat(t *testing.T) {
	tests := []struct {
		status rp.Status
		wire   []byte
	}{
		{rp.StatusOK, []byte{0x01, 0x00}},
		{rp.StatusRefused, []byte{0x01, 0x01}},
		{rp.StatusUnreachable, []byte{0x01, 0x02}},
		{rp.StatusTimeout, []byte{0x01, 0x03}},
		{rp.StatusPolicy, []byte{0x01, 0x04}},
	}
	for _, tt := range tests {
		t.Run(tt.status.String(), func(t *testing.T) {
			var buf bytes.Buffer
			if err := rp.WriteFlowResult(&buf, rp.FlowResult{Status: tt.status}); err != nil {
				t.Fatalf("WriteFlowResult: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), tt.wire) {
				t.Fatalf("wire = % x, want % x", buf.Bytes(), tt.wire)
			}
			got, err := rp.ReadFlowResult(&buf)
			if err != nil {
				t.Fatalf("ReadFlowResult: %v", err)
			}
			if got.Status != tt.status {
				t.Fatalf("ReadFlowResult status = %v, want %v", got.Status, tt.status)
			}
		})
	}
}

func TestReadFlowResultLeavesPayloadUnread(t *testing.T) {
	r := bytes.NewReader([]byte{0x01, 0x00, 0x00, 0x03, 'a', 'b', 'c'})
	if _, err := rp.ReadFlowResult(r); err != nil {
		t.Fatalf("ReadFlowResult: %v", err)
	}
	if r.Len() != 5 {
		t.Fatalf("ReadFlowResult left %d bytes, want 5", r.Len())
	}
}

func TestReadFlowResultRejectsMalformedResults(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
		want error
	}{
		{"empty stream", nil, io.EOF},
		{"unknown version", []byte{0x02, 0x00}, rp.ErrUnsupportedVersion},
		{"unknown status", []byte{0x01, 0x05}, rp.ErrMalformed},
		{"truncated", []byte{0x01}, io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rp.ReadFlowResult(bytes.NewReader(tt.wire))
			if !errors.Is(err, tt.want) {
				t.Fatalf("ReadFlowResult error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestWriteFlowResultRejectsUnknownStatus(t *testing.T) {
	var buf bytes.Buffer
	if err := rp.WriteFlowResult(&buf, rp.FlowResult{Status: 42}); !errors.Is(err, rp.ErrMalformed) {
		t.Fatalf("WriteFlowResult error = %v, want ErrMalformed", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("WriteFlowResult wrote % x for an unknown status", buf.Bytes())
	}
}

func TestFlowErrorsNeverNameTheDestination(t *testing.T) {
	// Callers log flow errors, and logs must never hold destinations.
	open := rp.FlowOpen{Kind: rp.KindTCP, Dst: netip.MustParseAddrPort("[::ffff:93.184.215.14]:443")}
	err := rp.WriteFlowOpen(io.Discard, open)
	if err == nil || strings.Contains(err.Error(), "93.184.215.14") {
		t.Fatalf("WriteFlowOpen error %q names the destination", err)
	}
	zoned := rp.FlowOpen{Kind: rp.KindTCP, Dst: netip.MustParseAddrPort("[fe80::abcd%en0]:443")}
	if err := rp.WriteFlowOpen(io.Discard, zoned); err == nil || strings.Contains(err.Error(), "abcd") {
		t.Fatalf("WriteFlowOpen error %q names the destination", err)
	}
	wire := []byte{0x01, 0x01, 0x06, 0x00, 0x01, 0xbb, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 93, 184, 215, 14}
	if _, err := rp.ReadFlowOpen(bytes.NewReader(wire)); err == nil || strings.Contains(err.Error(), "93.184.215.14") {
		t.Fatalf("ReadFlowOpen error %q names the destination", err)
	}
}
