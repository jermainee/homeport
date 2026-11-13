package relayprotocol

import (
	"fmt"

	"github.com/quic-go/quic-go"
)

// Code is an application error code the gateway and the agent use to close a
// QUIC connection (CONNECTION_CLOSE) or to reset or stop a stream
// (RESET_STREAM, STOP_SENDING).
type Code uint64

const (
	// CodeNoError closes a connection or stream normally.
	CodeNoError Code = 0x00
	// CodeProtocolError means the peer broke the wire format: a malformed
	// header, frame or control message, or a stream it may not open.
	CodeProtocolError Code = 0x01
	// CodeUnsupportedVersion resets a flow stream whose FlowOpen has a
	// version the agent does not support.
	CodeUnsupportedVersion Code = 0x02
	// CodeInternalError means the sender failed for its own reasons.
	CodeInternalError Code = 0x03
	// CodeUnauthorized closes the connection of an agent the gateway does
	// not accept, for example because the device was removed or its line is
	// not active.
	CodeUnauthorized Code = 0x04
	// CodeGoingAway closes a connection because the sender is shutting down
	// or pausing. The agent reconnects, to another gateway if needed.
	CodeGoingAway Code = 0x05
	// CodeFlowReset resets a TCP flow stream because one end of the relayed
	// connection was reset. The receiver resets its side (TCP RST) instead
	// of closing it with FIN.
	CodeFlowReset Code = 0x06
	// CodeFlowCancelled ends a flow stream early for any other reason, such
	// as the client going away, an exhausted lease or an idle UDP flow.
	CodeFlowCancelled Code = 0x07
)

var codeNames = map[Code]string{
	CodeNoError:            "NO_ERROR",
	CodeProtocolError:      "PROTOCOL_ERROR",
	CodeUnsupportedVersion: "UNSUPPORTED_VERSION",
	CodeInternalError:      "INTERNAL_ERROR",
	CodeUnauthorized:       "UNAUTHORIZED",
	CodeGoingAway:          "GOING_AWAY",
	CodeFlowReset:          "FLOW_RESET",
	CodeFlowCancelled:      "FLOW_CANCELLED",
}

func (c Code) String() string {
	if name, ok := codeNames[c]; ok {
		return name
	}
	return fmt.Sprintf("Code(%#x)", uint64(c))
}

// StreamErrorCode returns c for [quic.Stream.CancelRead] and
// [quic.Stream.CancelWrite].
func (c Code) StreamErrorCode() quic.StreamErrorCode { return quic.StreamErrorCode(c) }

// ApplicationErrorCode returns c for [quic.Conn.CloseWithError].
func (c Code) ApplicationErrorCode() quic.ApplicationErrorCode {
	return quic.ApplicationErrorCode(c)
}
