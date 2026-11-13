package relayprotocol

import (
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrPinMismatch reports a peer certificate whose SPKI pin is not allowed.
var ErrPinMismatch = errors.New("relayprotocol: certificate pin not allowed")

// Pin is the SHA-256 hash of a certificate's DER-encoded SubjectPublicKeyInfo,
// as in HPKP's pin-sha256. It identifies the key, so it survives re-issuing
// the certificate for the same key.
//
// Its text form is standard base64 with padding (44 characters), which is
// also how it appears in JSON.
type Pin [sha256.Size]byte

// PinOf returns the pin of cert's public key.
func PinOf(cert *x509.Certificate) Pin {
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo)
}

// PinOfPublicKey returns the pin of a public key, such as
// ed25519.PrivateKey.Public().
func PinOfPublicKey(pub crypto.PublicKey) (Pin, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return Pin{}, fmt.Errorf("relayprotocol: pin of public key: %w", err)
	}
	return sha256.Sum256(spki), nil
}

// PeerPin returns the pin of the certificate the peer presented in a TLS
// handshake. The gateway uses it to find the agent's device.
func PeerPin(cs tls.ConnectionState) (Pin, error) {
	if len(cs.PeerCertificates) == 0 {
		return Pin{}, errors.New("relayprotocol: peer presented no certificate")
	}
	return PinOf(cs.PeerCertificates[0]), nil
}

// ParsePin parses the text form of a pin.
func ParsePin(s string) (Pin, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != sha256.Size || base64.StdEncoding.EncodeToString(b) != s {
		return Pin{}, fmt.Errorf("relayprotocol: pin %q is not 44 characters of padded base64 encoding 32 bytes", s)
	}
	return Pin(b), nil
}

// String returns the text form of p.
func (p Pin) String() string { return base64.StdEncoding.EncodeToString(p[:]) }

// MarshalText implements [encoding.TextMarshaler].
func (p Pin) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// UnmarshalText implements [encoding.TextUnmarshaler].
func (p *Pin) UnmarshalText(text []byte) error {
	pin, err := ParsePin(string(text))
	if err != nil {
		return err
	}
	*p = pin
	return nil
}

// AllowPins returns an allow function for [VerifyPinned] that accepts
// exactly the given pins. With no pins it accepts nothing.
func AllowPins(pins ...Pin) func(Pin) bool {
	set := make(map[Pin]struct{}, len(pins))
	for _, p := range pins {
		set[p] = struct{}{}
	}
	return func(p Pin) bool {
		_, ok := set[p]
		return ok
	}
}

// VerifyPinned returns a [tls.Config] VerifyPeerCertificate callback that
// accepts a peer only if it presents exactly one certificate and allow
// accepts that certificate's pin. A nil allow accepts nothing.
//
// It replaces chain verification, which pinned self-signed certificates
// cannot pass, so it is meant for configs with InsecureSkipVerify on the
// client or RequireAnyClientCert on the server. TLS 1.3 still proves that the
// peer holds the certificate's private key. The callback does not run on
// resumed sessions, so such configs must also disable session tickets;
// [AgentTLSConfig] and [GatewayTLSConfig] do all of this.
func VerifyPinned(allow func(Pin) bool) func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) != 1 {
			return fmt.Errorf("relayprotocol: peer presented %d certificates, want one self-signed certificate", len(rawCerts))
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("relayprotocol: peer certificate: %w", err)
		}
		pin := PinOf(cert)
		if allow == nil || !allow(pin) {
			return fmt.Errorf("%w: %s", ErrPinMismatch, pin)
		}
		return nil
	}
}
