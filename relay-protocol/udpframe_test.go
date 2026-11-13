package relayprotocol_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	rp "github.com/jermainee/homeport/relay-protocol"
)

func TestUDPFrameWireFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := rp.WriteUDPFrame(&buf, []byte("hi!")); err != nil {
		t.Fatalf("WriteUDPFrame: %v", err)
	}
	want := []byte{0x00, 0x03, 'h', 'i', '!'}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("wire = % x, want % x", buf.Bytes(), want)
	}
}

func TestUDPFrameRoundTripsSmallestAndLargestPayload(t *testing.T) {
	largest := bytes.Repeat([]byte{0xa5}, 65535)
	largest[0], largest[65534] = 0x01, 0xfe
	tests := []struct {
		name       string
		payload    []byte
		wireHeader []byte
	}{
		{"empty datagram", []byte{}, []byte{0x00, 0x00}},
		{"65,535-byte datagram", largest, []byte{0xff, 0xff}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := rp.WriteUDPFrame(&buf, tt.payload); err != nil {
				t.Fatalf("WriteUDPFrame: %v", err)
			}
			if buf.Len() != 2+len(tt.payload) || !bytes.Equal(buf.Bytes()[:2], tt.wireHeader) {
				t.Fatalf("frame starts % x and is %d bytes, want % x and %d bytes",
					buf.Bytes()[:2], buf.Len(), tt.wireHeader, 2+len(tt.payload))
			}
			got, err := rp.ReadUDPFrame(&buf, make([]byte, rp.MaxUDPPayload))
			if err != nil {
				t.Fatalf("ReadUDPFrame: %v", err)
			}
			if !bytes.Equal(got, tt.payload) {
				t.Fatalf("ReadUDPFrame returned %d bytes, want the %d written", len(got), len(tt.payload))
			}
		})
	}
}

func TestWriteUDPFrameRejectsOversizedPayload(t *testing.T) {
	var buf bytes.Buffer
	err := rp.WriteUDPFrame(&buf, make([]byte, 65536))
	if !errors.Is(err, rp.ErrMalformed) {
		t.Fatalf("WriteUDPFrame error = %v, want ErrMalformed", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("WriteUDPFrame wrote %d bytes for an oversized payload", buf.Len())
	}
}

func TestUDPFrameWrittenInOneWrite(t *testing.T) {
	w := &countingWriter{}
	if err := rp.WriteUDPFrame(w, []byte("one datagram")); err != nil {
		t.Fatalf("WriteUDPFrame: %v", err)
	}
	if w.writes != 1 {
		t.Fatalf("WriteUDPFrame used %d writes, want 1", w.writes)
	}
}

func TestReadUDPFrameReadsFramesInOrder(t *testing.T) {
	stream := []byte{0x00, 0x01, 'a', 0x00, 0x00, 0x00, 0x02, 'b', 'c'}
	r := bytes.NewReader(stream)
	buf := make([]byte, rp.MaxUDPPayload)
	for _, want := range []string{"a", "", "bc"} {
		got, err := rp.ReadUDPFrame(r, buf)
		if err != nil {
			t.Fatalf("ReadUDPFrame: %v", err)
		}
		if string(got) != want {
			t.Fatalf("ReadUDPFrame = %q, want %q", got, want)
		}
	}
	if _, err := rp.ReadUDPFrame(r, buf); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadUDPFrame at end of stream = %v, want io.EOF", err)
	}
}

func TestReadUDPFrameGrowsSmallBuffer(t *testing.T) {
	r := bytes.NewReader([]byte{0x00, 0x05, 'h', 'e', 'l', 'l', 'o'})
	got, err := rp.ReadUDPFrame(r, make([]byte, 2))
	if err != nil {
		t.Fatalf("ReadUDPFrame: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("ReadUDPFrame = %q, want %q", got, "hello")
	}
}

func TestReadUDPFrameReusesLargeEnoughBuffer(t *testing.T) {
	buf := make([]byte, 16)
	got, err := rp.ReadUDPFrame(bytes.NewReader([]byte{0x00, 0x02, 'o', 'k'}), buf)
	if err != nil {
		t.Fatalf("ReadUDPFrame: %v", err)
	}
	if &got[0] != &buf[0] {
		t.Fatal("ReadUDPFrame allocated although the buffer was large enough")
	}
}

func TestReadUDPFrameRejectsTruncatedFrames(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
	}{
		{"half a length", []byte{0x00}},
		{"length without payload", []byte{0x00, 0x04}},
		{"short payload", []byte{0x00, 0x04, 'a', 'b', 'c'}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rp.ReadUDPFrame(bytes.NewReader(tt.wire), nil)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("ReadUDPFrame error = %v, want io.ErrUnexpectedEOF", err)
			}
		})
	}
}

func FuzzReadUDPFrame(f *testing.F) {
	f.Add([]byte{0x00, 0x00})
	f.Add([]byte{0x00, 0x03, 'h', 'i', '!', 0x00})
	f.Add([]byte{0xff, 0xff, 0x00})
	f.Add([]byte{0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		payload, err := rp.ReadUDPFrame(r, nil)
		if err != nil {
			return
		}
		consumed := data[:len(data)-r.Len()]
		var buf bytes.Buffer
		if err := rp.WriteUDPFrame(&buf, payload); err != nil {
			t.Fatalf("decoded a %d-byte payload that cannot be encoded: %v", len(payload), err)
		}
		if !bytes.Equal(buf.Bytes(), consumed) {
			t.Fatalf("decoded % x, which encodes as % x", consumed, buf.Bytes())
		}
	})
}

func TestWriteUDPFrameDoesNotAllocate(t *testing.T) {
	// It runs once per relayed datagram. Under -race, sync.Pool drops some
	// buffers on purpose; AllocsPerRun's whole-number average still rounds
	// those rare allocations down to 0.
	payload := make([]byte, 1350)
	allocs := testing.AllocsPerRun(100, func() {
		if err := rp.WriteUDPFrame(io.Discard, payload); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("WriteUDPFrame allocates %.1f times per call, want 0", allocs)
	}
}
