package relayprotocol_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	rp "github.com/jermainee/homeport/relay-protocol"
)

// controlFrame builds a control message the way another implementation
// would: a big-endian u32 length followed by the JSON text.
func controlFrame(jsonText string) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(jsonText))), jsonText...)
}

func TestControlMessagesRoundTrip(t *testing.T) {
	msgs := []rp.ControlMessage{
		rp.Hello{AgentVersion: "0.1.0", OS: "darwin"},
		rp.HelloAck{LineID: "line_01J9Z3", ObservedIP: netip.MustParseAddr("2001:db8::42")},
		rp.HelloAck{LineID: "line_01J9Z3", ObservedIP: netip.MustParseAddr("198.51.100.23")},
		rp.Stats{FlowsActive: 3, FlowsTotal: 1200, BytesOut: 5_000_000_000, BytesIn: 1 << 40},
		rp.Stats{},
	}
	for _, msg := range msgs {
		var buf bytes.Buffer
		if err := rp.WriteControl(&buf, msg); err != nil {
			t.Fatalf("WriteControl(%#v): %v", msg, err)
		}
		got, err := rp.ReadControl(&buf)
		if err != nil {
			t.Fatalf("ReadControl(%#v): %v", msg, err)
		}
		if !reflect.DeepEqual(got, msg) {
			t.Fatalf("round trip = %#v, want %#v", got, msg)
		}
	}
}

func TestControlWireFormat(t *testing.T) {
	tests := []struct {
		msg  rp.ControlMessage
		want map[string]any
	}{
		{
			rp.Hello{AgentVersion: "0.1.0", OS: "windows"},
			map[string]any{"type": "hello", "agent_version": "0.1.0", "os": "windows"},
		},
		{
			rp.HelloAck{LineID: "line_7", ObservedIP: netip.MustParseAddr("203.0.113.9")},
			map[string]any{"type": "hello_ack", "line_id": "line_7", "observed_ip": "203.0.113.9"},
		},
		{
			rp.Stats{FlowsActive: 2, FlowsTotal: 9, BytesOut: 100, BytesIn: 2500},
			map[string]any{"type": "stats", "flows_active": 2.0, "flows_total": 9.0, "bytes_out": 100.0, "bytes_in": 2500.0},
		},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		if err := rp.WriteControl(&buf, tt.msg); err != nil {
			t.Fatalf("WriteControl: %v", err)
		}
		wire := buf.Bytes()
		if n := binary.BigEndian.Uint32(wire[:4]); int(n) != len(wire)-4 {
			t.Fatalf("length prefix %d, want %d", n, len(wire)-4)
		}
		var got map[string]any
		if err := json.Unmarshal(wire[4:], &got); err != nil {
			t.Fatalf("payload %q is not JSON: %v", wire[4:], err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("payload = %v, want %v", got, tt.want)
		}
	}
}

func TestReadControlIgnoresUnknownFields(t *testing.T) {
	r := bytes.NewReader(controlFrame(`{"type":"hello","agent_version":"0.2.0","os":"linux","new_field":[1,2]}`))
	got, err := rp.ReadControl(r)
	if err != nil {
		t.Fatalf("ReadControl: %v", err)
	}
	if want := (rp.Hello{AgentVersion: "0.2.0", OS: "linux"}); got != want {
		t.Fatalf("ReadControl = %#v, want %#v", got, want)
	}
}

func TestReadControlSkipsUnknownTypesAndStaysInSync(t *testing.T) {
	stream := append(controlFrame(`{"type":"speed_report","mbit":40}`), controlFrame(`{"type":"stats","flows_active":1}`)...)
	r := bytes.NewReader(stream)

	if _, err := rp.ReadControl(r); !errors.Is(err, rp.ErrUnknownMessage) {
		t.Fatalf("ReadControl error = %v, want ErrUnknownMessage", err)
	}
	got, err := rp.ReadControl(r)
	if err != nil {
		t.Fatalf("ReadControl after unknown type: %v", err)
	}
	if want := (rp.Stats{FlowsActive: 1}); got != want {
		t.Fatalf("ReadControl = %#v, want %#v", got, want)
	}
}

func TestReadControlRejectsMalformedMessages(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
		want error
	}{
		{"empty stream", nil, io.EOF},
		{"truncated length", []byte{0x00, 0x00}, io.ErrUnexpectedEOF},
		{"truncated body", controlFrame(`{"type":"stats"}`)[:8], io.ErrUnexpectedEOF},
		{"zero length", []byte{0, 0, 0, 0}, rp.ErrMalformed},
		{"not JSON", controlFrame(`hello`), rp.ErrMalformed},
		{"JSON array", controlFrame(`[1,2]`), rp.ErrMalformed},
		{"no type", controlFrame(`{"line_id":"line_7"}`), rp.ErrMalformed},
		{"wrong field type", controlFrame(`{"type":"stats","bytes_in":-1}`), rp.ErrMalformed},
		{"hello_ack without line", controlFrame(`{"type":"hello_ack","observed_ip":"203.0.113.9"}`), rp.ErrMalformed},
		{"hello_ack without IP", controlFrame(`{"type":"hello_ack","line_id":"line_7"}`), rp.ErrMalformed},
		{"hello_ack with bad IP", controlFrame(`{"type":"hello_ack","line_id":"line_7","observed_ip":"home"}`), rp.ErrMalformed},
		{"hello_ack with IPv4-mapped IP", controlFrame(`{"type":"hello_ack","line_id":"line_7","observed_ip":"::ffff:203.0.113.9"}`), rp.ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rp.ReadControl(bytes.NewReader(tt.wire))
			if !errors.Is(err, tt.want) {
				t.Fatalf("ReadControl error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestReadControlRejectsOversizedLengthBeforeReadingBody(t *testing.T) {
	// The length alone must be enough to reject the message, so a peer
	// cannot make the reader wait for or buffer a huge body.
	wire := binary.BigEndian.AppendUint32(nil, rp.MaxControlLen+1)
	_, err := rp.ReadControl(bytes.NewReader(wire))
	if !errors.Is(err, rp.ErrMalformed) {
		t.Fatalf("ReadControl error = %v, want ErrMalformed", err)
	}
}

func TestWriteControlRejectsInvalidMessages(t *testing.T) {
	tests := []struct {
		name string
		msg  rp.ControlMessage
	}{
		{"nil", nil},
		{"hello_ack without line", rp.HelloAck{ObservedIP: netip.MustParseAddr("203.0.113.9")}},
		{"hello_ack without IP", rp.HelloAck{LineID: "line_7"}},
		{"hello_ack with IPv4-mapped IP", rp.HelloAck{LineID: "line_7", ObservedIP: netip.MustParseAddr("::ffff:203.0.113.9")}},
		{"hello too large", rp.Hello{AgentVersion: strings.Repeat("9", rp.MaxControlLen)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := rp.WriteControl(&buf, tt.msg); !errors.Is(err, rp.ErrMalformed) {
				t.Fatalf("WriteControl error = %v, want ErrMalformed", err)
			}
			if buf.Len() != 0 {
				t.Fatalf("WriteControl wrote %d bytes for an invalid message", buf.Len())
			}
		})
	}
}

func TestControlMessageWrittenInOneWrite(t *testing.T) {
	w := &countingWriter{}
	if err := rp.WriteControl(w, rp.Stats{BytesIn: 1}); err != nil {
		t.Fatalf("WriteControl: %v", err)
	}
	if w.writes != 1 {
		t.Fatalf("WriteControl used %d writes, want 1", w.writes)
	}
}
