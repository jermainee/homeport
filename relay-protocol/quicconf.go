package relayprotocol

import (
	"crypto/tls"
	"errors"
	"time"

	"github.com/quic-go/quic-go"
)

// ALPN is the TLS application protocol of version 1 of the relay protocol.
// A peer that does not offer it fails the handshake.
const ALPN = "homeport-relay/1"

const (
	// KeepAlivePeriod is how often each side sends a keep-alive packet, so
	// the NAT in front of the agent keeps the connection's UDP mapping.
	KeepAlivePeriod = 15 * time.Second
	// MaxIdleTimeout closes a connection that received nothing for this
	// long, which is four missed keep-alives.
	MaxIdleTimeout = 60 * time.Second
	// AgentMaxIncomingStreams is how many flow streams the gateway may have
	// open to one agent at once. Each one holds an OS socket on the agent,
	// so the agent must allow at least this many open files.
	AgentMaxIncomingStreams = 1024
	// GatewayMaxIncomingStreams lets the agent open only its control stream.
	GatewayMaxIncomingStreams = 1
)

// AgentQUICConfig returns a new QUIC config for the agent, which dials the
// gateway and accepts one stream per flow.
func AgentQUICConfig() *quic.Config {
	return &quic.Config{
		KeepAlivePeriod:       KeepAlivePeriod,
		MaxIdleTimeout:        MaxIdleTimeout,
		MaxIncomingStreams:    AgentMaxIncomingStreams,
		MaxIncomingUniStreams: -1,
	}
}

// GatewayQUICConfig returns a new QUIC config for the gateway's listener. An
// agent may open only its control stream.
func GatewayQUICConfig() *quic.Config {
	return &quic.Config{
		KeepAlivePeriod:       KeepAlivePeriod,
		MaxIdleTimeout:        MaxIdleTimeout,
		MaxIncomingStreams:    GatewayMaxIncomingStreams,
		MaxIncomingUniStreams: -1,
	}
}

// AgentTLSConfig returns a new TLS config for the agent. The agent presents
// cert as its client certificate and accepts the gateway only if it
// negotiates [ALPN] and allowGateway accepts the pin of its certificate.
func AgentTLSConfig(cert tls.Certificate, allowGateway func(Pin) bool) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{ALPN},
		Certificates: []tls.Certificate{cert},
		// Pinning replaces chain and hostname verification.
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection checks the pin on every handshake.
		VerifyConnection:   verifyRelayConnection(allowGateway),
		// Nothing needs session resumption, so it stays off.
		SessionTicketsDisabled: true,
	}
}

// GatewayTLSConfig returns a new TLS config for the gateway. The gateway
// presents cert and requires [ALPN] and a client certificate whose pin
// allowAgent accepts. allowAgent runs on every handshake, so it can read a
// pin list that changes at run time; it must be safe for concurrent use.
func GatewayTLSConfig(cert tls.Certificate, allowAgent func(Pin) bool) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{ALPN},
		Certificates: []tls.Certificate{cert},
		// RequireAnyClientCert skips chain verification, so
		// VerifyConnection is the only check of the agent.
		ClientAuth:             tls.RequireAnyClientCert,
		VerifyConnection:       verifyRelayConnection(allowAgent),
		SessionTicketsDisabled: true,
	}
}

// codeBadCertificate is the QUIC transport error that carries a TLS
// bad_certificate alert: 0x100 plus the alert's number, 42 (RFC 9001,
// section 4.8).
const codeBadCertificate quic.TransportErrorCode = 0x100 + 42

// CertificateRefused reports whether err ended an agent's connection
// because the gateway refused its certificate during the handshake. A
// gateway refuses every pin its config does not list, and its config may
// simply be older than the agent's line.
func CertificateRefused(err error) bool {
	var te *quic.TransportError
	return errors.As(err, &te) && te.Remote && te.ErrorCode == codeBadCertificate
}
