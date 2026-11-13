package relayprotocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// noExpiry is the GeneralizedTime RFC 5280, section 4.1.2.5, reserves for a
// certificate without a well-defined expiration date. Trust rests on the pin
// of the key, not on the certificate's validity period, and neither side
// checks it.
var noExpiry = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

// GenerateSelfSigned creates a new Ed25519 key and a self-signed certificate
// for it. Both the agent and the gateway use one as their TLS identity.
func GenerateSelfSigned(commonName string) (tls.Certificate, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("relayprotocol: generate Ed25519 key: %w", err)
	}
	return NewSelfSigned(key, commonName)
}

// NewSelfSigned creates a self-signed certificate for an existing Ed25519
// key. The pin depends only on the key, so a certificate rebuilt from a
// stored key keeps the pin the peer already trusts.
func NewSelfSigned(key ed25519.PrivateKey, commonName string) (tls.Certificate, error) {
	if len(key) != ed25519.PrivateKeySize {
		return tls.Certificate{}, fmt.Errorf("relayprotocol: Ed25519 private key is %d bytes, want %d", len(key), ed25519.PrivateKeySize)
	}
	template := &x509.Certificate{
		// A nil SerialNumber makes CreateCertificate pick a random one.
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour).UTC(),
		NotAfter:              noExpiry,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("relayprotocol: create certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("relayprotocol: parse new certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// MarshalPEM encodes a certificate from [GenerateSelfSigned] as a PEM
// CERTIFICATE block and its key as a PEM PKCS #8 PRIVATE KEY block, the
// format [tls.X509KeyPair] and [tls.LoadX509KeyPair] read back.
func MarshalPEM(cert tls.Certificate) (certPEM, keyPEM []byte, err error) {
	if len(cert.Certificate) == 0 {
		return nil, nil, errors.New("relayprotocol: certificate is empty")
	}
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("relayprotocol: encode private key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	return certPEM, keyPEM, nil
}
