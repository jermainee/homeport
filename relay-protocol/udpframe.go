package relayprotocol

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// MaxUDPPayload is the largest payload a UDP frame can carry.
const MaxUDPPayload = 65535

// udpFrameHeaderLen is the length prefix in front of each payload.
const udpFrameHeaderLen = 2

// AppendUDPFrame appends one UDP frame carrying payload to b. A UDP frame is
//
//	length u16 | payload (length bytes)
//
// with a big-endian length from 0 to [MaxUDPPayload]. On KindUDP flows each
// frame is one datagram; on KindDNS flows each frame is one DNS message.
func AppendUDPFrame(b, payload []byte) ([]byte, error) {
	if len(payload) > MaxUDPPayload {
		return b, fmt.Errorf("%w: UDP payload of %d bytes exceeds %d", ErrMalformed, len(payload), MaxUDPPayload)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(payload))) //nolint:gosec // At most MaxUDPPayload, checked above.
	return append(b, payload...), nil
}

// framePool holds buffers for WriteUDPFrame, which runs once per relayed
// datagram. They start at a typical datagram's size and keep what they grow
// to.
var framePool = sync.Pool{New: func() any {
	b := make([]byte, 0, udpFrameHeaderLen+1500)
	return &b
}}

// WriteUDPFrame writes one UDP frame carrying payload to w in a single Write
// call, using a pooled buffer.
func WriteUDPFrame(w io.Writer, payload []byte) error {
	bp := framePool.Get().(*[]byte)
	b, err := AppendUDPFrame((*bp)[:0], payload)
	if err == nil {
		_, err = w.Write(b)
	}
	*bp = b[:0]
	framePool.Put(bp)
	return err
}

// ReadUDPFrame reads one UDP frame from r and returns its payload. The
// payload is read into buf when it fits in buf's capacity, so passing a buffer
// of [MaxUDPPayload] bytes avoids allocation; otherwise a new slice is
// allocated.
//
// It returns [io.EOF] if r ends cleanly before a frame and
// [io.ErrUnexpectedEOF] if r ends inside one.
func ReadUDPFrame(r io.Reader, buf []byte) ([]byte, error) {
	var hdr [udpFrameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if err := readFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
