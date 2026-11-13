package controlplane

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// newUUIDv7 returns a lowercase UUIDv7 with t's millisecond timestamp. The
// API orders and expires request and event IDs by that timestamp.
func newUUIDv7(t time.Time) string {
	var b [16]byte
	_, _ = rand.Read(b[6:]) // crypto/rand.Read never fails
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(t.UnixMilli())) // #nosec G115 -- times are after 1970
	copy(b[:6], ts[2:])
	b[6] = 0x70 | b[6]&0x0f // version 7
	b[8] = 0x80 | b[8]&0x3f // RFC 9562 variant
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:36], b[10:16])
	return string(s[:])
}

// timestamp formats t as RFC 3339 in UTC with milliseconds, the precision
// the API keeps.
func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}
