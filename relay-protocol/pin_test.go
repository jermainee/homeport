package relayprotocol_test

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"testing"

	rp "github.com/jermainee/homeport/relay-protocol"
)

// seedKey is the Ed25519 key with seed 00 01 02 … 1f. Its pin was computed
// independently with OpenSSL:
//
//	printf 302e020100300506032b657004220420000102…1f | xxd -r -p |
//	  openssl pkey -inform DER -pubout -outform DER |
//	  openssl dgst -sha256 -binary | base64
var seedKey = ed25519.NewKeyFromSeed([]byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
})

const seedKeyPin = "oFCDfYUHBYLM9zlLCYiEfMMSy4glm4lImfbyOc8XkaU="

func TestPinOfPublicKeyMatchesOpenSSL(t *testing.T) {
	pin, err := rp.PinOfPublicKey(seedKey.Public())
	if err != nil {
		t.Fatalf("PinOfPublicKey: %v", err)
	}
	if pin.String() != seedKeyPin {
		t.Fatalf("pin = %s, want %s", pin, seedKeyPin)
	}
}

func TestPinOfCertificateIsThePinOfItsKey(t *testing.T) {
	cert, err := rp.NewSelfSigned(seedKey, "agent")
	if err != nil {
		t.Fatalf("NewSelfSigned: %v", err)
	}
	if got := rp.PinOf(cert.Leaf).String(); got != seedKeyPin {
		t.Fatalf("PinOf(cert) = %s, want %s", got, seedKeyPin)
	}
}

func TestParsePin(t *testing.T) {
	pin, err := rp.ParsePin(seedKeyPin)
	if err != nil {
		t.Fatalf("ParsePin: %v", err)
	}
	if pin.String() != seedKeyPin {
		t.Fatalf("ParsePin(%s).String() = %s", seedKeyPin, pin)
	}
	for _, bad := range []string{
		"",
		"oFCDfYUHBYLM9zlLCYiEfMMSy4glm4lImfbyOc8XkaU",      // unpadded
		"oFCDfYUHBYLM9zlLCYiEfMMSy4glm4lImfbyOc8Xka==",     // 31 bytes
		"oFCDfYUHBYLM9zlLCYiEfMMSy4glm4lImfbyOc8XkaUA",     // 33 bytes
		"oFCDfYUHBYLM9zlLCYiEfMMSy4glm4lImfbyOc8Xk!U=",     // not base64
		"a050837d85070582ccf7394b09887c7cc312cb88259b8948", // hex
	} {
		if _, err := rp.ParsePin(bad); err == nil {
			t.Errorf("ParsePin(%q) succeeded, want an error", bad)
		}
	}
}

func TestPinIsAJSONString(t *testing.T) {
	pin, _ := rp.ParsePin(seedKeyPin)
	b, err := json.Marshal(map[string]rp.Pin{"spki": pin})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `{"spki":"` + seedKeyPin + `"}`; string(b) != want {
		t.Fatalf("JSON = %s, want %s", b, want)
	}
	var back map[string]rp.Pin
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back["spki"] != pin {
		t.Fatalf("Unmarshal = %s, want %s", back["spki"], pin)
	}
}

func TestVerifyPinnedAcceptsOnlyAllowedCertificates(t *testing.T) {
	trusted := mustGenerate(t, "gateway")
	stranger := mustGenerate(t, "gateway")
	verify := rp.VerifyPinned(rp.AllowPins(rp.PinOf(trusted.Leaf)))

	if err := verify(trusted.Certificate, nil); err != nil {
		t.Fatalf("pinned certificate rejected: %v", err)
	}
	err := verify(stranger.Certificate, nil)
	if !errors.Is(err, rp.ErrPinMismatch) {
		t.Fatalf("unpinned certificate: error = %v, want ErrPinMismatch", err)
	}
}

func TestVerifyPinnedRejectsMalformedPeerCertificates(t *testing.T) {
	trusted := mustGenerate(t, "agent")
	other := mustGenerate(t, "agent")
	verify := rp.VerifyPinned(rp.AllowPins(rp.PinOf(trusted.Leaf)))

	tests := []struct {
		name     string
		rawCerts [][]byte
	}{
		{"no certificate", nil},
		{"certificate chain", [][]byte{trusted.Certificate[0], other.Certificate[0]}},
		{"garbage", [][]byte{[]byte("not a certificate")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := verify(tt.rawCerts, nil); err == nil {
				t.Fatal("VerifyPinned accepted it")
			}
		})
	}
}

func TestAllowPinsWithNoPinsAllowsNothing(t *testing.T) {
	cert := mustGenerate(t, "agent")
	if rp.AllowPins()(rp.PinOf(cert.Leaf)) {
		t.Fatal("AllowPins() allowed a pin")
	}
}

func TestGenerateSelfSignedMakesEd25519Certificates(t *testing.T) {
	cert := mustGenerate(t, "homeport-agent")

	if _, ok := cert.PrivateKey.(ed25519.PrivateKey); !ok {
		t.Fatalf("private key is %T, want ed25519.PrivateKey", cert.PrivateKey)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if leaf.Subject.CommonName != "homeport-agent" {
		t.Fatalf("common name = %q", leaf.Subject.CommonName)
	}
	if leaf.PublicKeyAlgorithm != x509.Ed25519 {
		t.Fatalf("public key algorithm = %v, want Ed25519", leaf.PublicKeyAlgorithm)
	}
	if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
		t.Fatalf("certificate is not self-signed: %v", err)
	}
	if rp.PinOf(leaf) != rp.PinOf(cert.Leaf) {
		t.Fatal("Leaf does not match Certificate[0]")
	}
	if other := mustGenerate(t, "homeport-agent"); rp.PinOf(other.Leaf) == rp.PinOf(leaf) {
		t.Fatal("two generated certificates share a key")
	}
}

func TestNewSelfSignedKeepsThePinForTheSameKey(t *testing.T) {
	// The agent may store only its private key and rebuild the certificate
	// at start-up; the pin the API registered must not change.
	a, err := rp.NewSelfSigned(seedKey, "agent")
	if err != nil {
		t.Fatalf("NewSelfSigned: %v", err)
	}
	b, err := rp.NewSelfSigned(seedKey, "agent")
	if err != nil {
		t.Fatalf("NewSelfSigned: %v", err)
	}
	if rp.PinOf(a.Leaf) != rp.PinOf(b.Leaf) {
		t.Fatal("pins differ for the same key")
	}
}

func TestMarshalPEMLoadsWithTheStandardLibrary(t *testing.T) {
	cert := mustGenerate(t, "gateway")
	certPEM, keyPEM, err := rp.MarshalPEM(cert)
	if err != nil {
		t.Fatalf("MarshalPEM: %v", err)
	}
	loaded, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("tls.X509KeyPair: %v", err)
	}
	if rp.PinOf(loaded.Leaf) != rp.PinOf(cert.Leaf) {
		t.Fatal("loaded certificate has another pin")
	}
	if !loaded.PrivateKey.(ed25519.PrivateKey).Equal(cert.PrivateKey) {
		t.Fatal("loaded private key differs")
	}
}

func mustGenerate(t *testing.T, commonName string) tls.Certificate {
	t.Helper()
	cert, err := rp.GenerateSelfSigned(commonName)
	if err != nil {
		t.Fatalf("GenerateSelfSigned: %v", err)
	}
	return cert
}
