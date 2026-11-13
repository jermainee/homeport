// Package relayprotocol is the wire format between the Homeport edge gateway
// and a member's sharing agent.
//
// The agent dials out to the gateway over QUIC with mutual TLS, where each
// side pins the other's self-signed Ed25519 certificate by its SPKI hash. The
// agent then opens one bidirectional control stream (see [WriteControl]), and
// the gateway opens one bidirectional stream per relayed flow, starting with a
// [FlowOpen] header that the agent answers with a [FlowResult]. README.md has
// the full specification.
package relayprotocol

import "errors"

// Version is the version of the FlowOpen and FlowResult headers. Each header
// starts with it, and a reader rejects any other value.
const Version = 1

var (
	// ErrMalformed reports bytes that do not follow the wire format, or a
	// value that cannot be encoded in it.
	ErrMalformed = errors.New("relayprotocol: malformed message")

	// ErrUnsupportedVersion reports a header with a version other than
	// [Version]. The reader stops right after the version byte.
	ErrUnsupportedVersion = errors.New("relayprotocol: unsupported version")
)
