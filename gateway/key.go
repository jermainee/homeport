package gateway

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// Key is a Curve25519 key as WireGuard uses it, private or public.
type Key [32]byte

// GenerateKey returns a new, clamped WireGuard private key.
func GenerateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return Key{}, fmt.Errorf("generate key: %w", err)
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	return k, nil
}

// ParseKey reads a key in WireGuard's base64 form.
func ParseKey(s string) (Key, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != len(Key{}) {
		return Key{}, fmt.Errorf("invalid WireGuard key")
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

// PublicKey returns the public key that belongs to private key k.
func (k Key) PublicKey() Key {
	priv, err := ecdh.X25519().NewPrivateKey(k[:])
	if err != nil {
		// Every 32-byte string is a valid X25519 private key.
		panic(err)
	}
	var pub Key
	copy(pub[:], priv.PublicKey().Bytes())
	return pub
}

// Bytes returns the raw 32 bytes.
func (k Key) Bytes() []byte { return k[:] }

// String returns the key in WireGuard's base64 form.
func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }
