package relayprotocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
)

// MaxControlLen is the largest JSON body a control message may have.
const MaxControlLen = 64 << 10

// controlHeaderLen is the length prefix in front of each JSON body.
const controlHeaderLen = 4

// ErrUnknownMessage reports a well-formed control message of a type this
// version does not know. The whole message has been read, so the stream is
// still in sync and the receiver can skip it and read the next one.
var ErrUnknownMessage = errors.New("relayprotocol: unknown control message type")

// ControlMessage is a message on the control stream: [Hello], [HelloAck] or
// [Stats].
//
// On the wire each message is
//
//	length u32 | JSON object (length bytes)
//
// with a big-endian length from 1 to [MaxControlLen]. The object's "type"
// field names the message; its other fields are the message's fields.
// Receivers ignore unknown fields.
type ControlMessage interface {
	controlType() string
	validate() error
}

// Hello is the first message the agent sends on its control stream.
type Hello struct {
	// AgentVersion is the agent's release version, such as "0.1.0".
	AgentVersion string `json:"agent_version"`
	// OS is the agent's operating system as Go's GOOS names it: "darwin",
	// "linux" or "windows".
	OS string `json:"os"`
}

// HelloAck is the gateway's answer to Hello.
type HelloAck struct {
	// LineID is the line this agent serves, found from the SPKI pin of its
	// client certificate.
	LineID string `json:"line_id"`
	// ObservedIP is the agent's public IP address as the gateway sees it.
	// The agent refuses flows to it, so a line never relays to itself.
	ObservedIP netip.Addr `json:"observed_ip"`
}

// Stats is the agent's own count of the traffic it relayed, sent
// periodically on the control stream. The counters are cumulative since the
// control stream opened. They hold session metadata only: never destinations
// or content.
type Stats struct {
	// FlowsActive is the number of flows open now.
	FlowsActive uint64 `json:"flows_active"`
	// FlowsTotal is the number of flows the agent opened.
	FlowsTotal uint64 `json:"flows_total"`
	// BytesOut is the number of payload bytes the agent sent to
	// destinations. Payload excludes TCP, UDP and IP headers, which the
	// gateway's meter counts.
	BytesOut uint64 `json:"bytes_out"`
	// BytesIn is the number of payload bytes the agent received from
	// destinations.
	BytesIn uint64 `json:"bytes_in"`
}

const (
	typeHello    = "hello"
	typeHelloAck = "hello_ack"
	typeStats    = "stats"
)

func (Hello) controlType() string    { return typeHello }
func (HelloAck) controlType() string { return typeHelloAck }
func (Stats) controlType() string    { return typeStats }

func (Hello) validate() error { return nil }
func (Stats) validate() error { return nil }

func (m HelloAck) validate() error {
	switch {
	case m.LineID == "":
		return fmt.Errorf("%w: hello_ack without line_id", ErrMalformed)
	case !m.ObservedIP.IsValid():
		return fmt.Errorf("%w: hello_ack without observed_ip", ErrMalformed)
	case m.ObservedIP.Is4In6():
		return fmt.Errorf("%w: hello_ack observed_ip is IPv4-mapped; send it as IPv4", ErrMalformed)
	case m.ObservedIP.Zone() != "":
		return fmt.Errorf("%w: hello_ack observed_ip has a zone", ErrMalformed)
	}
	return nil
}

// WriteControl writes m to w in a single Write call. It accepts pointers to
// messages too, and sends an IPv4-mapped HelloAck.ObservedIP, as a gateway
// on a dual-stack socket sees IPv4 agents, as plain IPv4.
func WriteControl(w io.Writer, m ControlMessage) error {
	m, err := canonical(m)
	if err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
	}
	body, err := marshalControl(m)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if len(body) > MaxControlLen {
		return fmt.Errorf("%w: %s message of %d bytes exceeds %d", ErrMalformed, m.controlType(), len(body), MaxControlLen)
	}
	b := make([]byte, controlHeaderLen, controlHeaderLen+len(body))
	binary.BigEndian.PutUint32(b, uint32(len(body))) //nolint:gosec // At most MaxControlLen, checked above.
	_, err = w.Write(append(b, body...))
	return err
}

// canonical returns m as a value in its wire form. Pointers satisfy
// ControlMessage through their method sets, so they are dereferenced; nil
// is rejected.
func canonical(m ControlMessage) (ControlMessage, error) {
	switch v := m.(type) {
	case Hello, Stats:
		return v, nil
	case HelloAck:
		v.ObservedIP = v.ObservedIP.Unmap()
		return v, nil
	case *Hello:
		if v != nil {
			return *v, nil
		}
	case *HelloAck:
		if v != nil {
			return canonical(*v)
		}
	case *Stats:
		if v != nil {
			return *v, nil
		}
	}
	return nil, fmt.Errorf("%w: nil control message", ErrMalformed)
}

// marshalControl encodes m as a JSON object with its type in the "type"
// field. Embedding the message promotes its fields next to "type".
func marshalControl(m ControlMessage) ([]byte, error) {
	switch m := m.(type) {
	case Hello:
		return json.Marshal(struct {
			Type string `json:"type"`
			Hello
		}{typeHello, m})
	case HelloAck:
		return json.Marshal(struct {
			Type string `json:"type"`
			HelloAck
		}{typeHelloAck, m})
	case Stats:
		return json.Marshal(struct {
			Type string `json:"type"`
			Stats
		}{typeStats, m})
	default:
		return nil, fmt.Errorf("unsupported control message %T", m)
	}
}

// ReadControl reads one control message from r.
//
// It returns [io.EOF] if r ends cleanly before a message,
// [io.ErrUnexpectedEOF] if r ends inside one, [ErrUnknownMessage] for a type
// it does not know and [ErrMalformed] for an invalid message. A length above
// [MaxControlLen] is rejected before any of the body is read.
func ReadControl(r io.Reader) (ControlMessage, error) {
	var hdr [controlHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxControlLen {
		return nil, fmt.Errorf("%w: control message length %d outside 1..%d", ErrMalformed, n, MaxControlLen)
	}
	body := make([]byte, n)
	if err := readFull(r, body); err != nil {
		return nil, err
	}

	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("%w: control message: %w", ErrMalformed, err)
	}
	var (
		m   ControlMessage
		err error
	)
	switch envelope.Type {
	case typeHello:
		m, err = unmarshalAs[Hello](body)
	case typeHelloAck:
		m, err = unmarshalAs[HelloAck](body)
	case typeStats:
		m, err = unmarshalAs[Stats](body)
	case "":
		return nil, fmt.Errorf("%w: control message without type", ErrMalformed)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownMessage, envelope.Type)
	}
	if err != nil {
		return nil, err
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

func unmarshalAs[M ControlMessage](body []byte) (ControlMessage, error) {
	var m M
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%w: %s message: %w", ErrMalformed, m.controlType(), err)
	}
	return m, nil
}
