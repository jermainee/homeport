package relayprotocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
)

// Kind is the transport of a relayed flow.
type Kind uint8

const (
	// KindTCP relays one TCP connection. After an OK result, the stream
	// carries the raw byte stream in both directions.
	KindTCP Kind = 1
	// KindUDP relays one UDP flow. After an OK result, the stream carries
	// UDP frames (see [WriteUDPFrame]) in both directions.
	KindUDP Kind = 2
	// KindDNS relays DNS queries that the agent answers through its system
	// resolver. Each UDP frame holds one DNS message. It carries both DNS
	// over UDP and DNS over TCP to the virtual resolver: DNS over TCP's own
	// 2-byte length prefix is the UDP frame format, so its byte stream
	// passes through unchanged.
	KindDNS Kind = 3
)

func (k Kind) String() string {
	switch k {
	case KindTCP:
		return "TCP"
	case KindUDP:
		return "UDP"
	case KindDNS:
		return "DNS"
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

func (k Kind) valid() bool { return k == KindTCP || k == KindUDP || k == KindDNS }

// Address family values on the wire.
const (
	familyIPv4 = 4
	familyIPv6 = 6
)

const (
	// flowOpenFixedLen is version, kind, family, flags and port.
	flowOpenFixedLen = 6
	// MaxFlowOpenLen is the length of the longest FlowOpen header, which
	// carries an IPv6 address.
	MaxFlowOpenLen = flowOpenFixedLen + 16
)

// FlowOpen is the header the gateway writes at the start of each flow stream.
//
// On the wire it is:
//
//	version u8 | kind u8 | family u8 | flags u8 | port u16 | address (4 or 16 bytes)
//
// Integers are big-endian. The family is 4 or 6 and sets the address length.
// No flags are defined in version 1, so flags must be zero.
type FlowOpen struct {
	Kind Kind
	// Dst is the destination the client addressed. For KindDNS it is the
	// virtual resolver address; the agent does not connect to it.
	Dst netip.AddrPort
}

// AppendBinary appends the wire encoding of f to b. It implements
// [encoding.BinaryAppender].
func (f FlowOpen) AppendBinary(b []byte) ([]byte, error) {
	if err := f.validate(); err != nil {
		return b, err
	}
	addr := f.Dst.Addr()
	if addr.Is4() {
		b = append(b, Version, byte(f.Kind), familyIPv4, 0)
		b = binary.BigEndian.AppendUint16(b, f.Dst.Port())
		a := addr.As4()
		return append(b, a[:]...), nil
	}
	b = append(b, Version, byte(f.Kind), familyIPv6, 0)
	b = binary.BigEndian.AppendUint16(b, f.Dst.Port())
	a := addr.As16()
	return append(b, a[:]...), nil
}

func (f FlowOpen) validate() error {
	if err := checkKindPort(f.Kind, f.Dst.Port()); err != nil {
		return err
	}
	return checkAddr(f.Dst.Addr())
}

// checkKindPort checks the fixed-size fields of a FlowOpen.
func checkKindPort(kind Kind, port uint16) error {
	switch {
	case !kind.valid():
		return fmt.Errorf("%w: unknown flow kind %d", ErrMalformed, kind)
	case port == 0:
		return fmt.Errorf("%w: destination port 0", ErrMalformed)
	}
	return nil
}

// checkAddr checks a FlowOpen destination address. Its errors never contain
// the address, because callers log them and logs never hold destinations.
func checkAddr(addr netip.Addr) error {
	switch {
	case !addr.IsValid():
		return fmt.Errorf("%w: flow has no destination address", ErrMalformed)
	case addr.Is4In6():
		return fmt.Errorf("%w: IPv4-mapped IPv6 destination; send it as IPv4", ErrMalformed)
	case addr.Zone() != "":
		return fmt.Errorf("%w: destination address has a zone", ErrMalformed)
	}
	return nil
}

// WriteFlowOpen writes f to w in a single Write call.
func WriteFlowOpen(w io.Writer, f FlowOpen) error {
	var buf [MaxFlowOpenLen]byte
	b, err := f.AppendBinary(buf[:0])
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadFlowOpen reads one FlowOpen header from r. It reads exactly the header's
// bytes, so payload that follows it stays in r.
//
// It returns [io.EOF] if r ends before the first byte, [io.ErrUnexpectedEOF]
// if r ends inside the header, [ErrUnsupportedVersion] for another version
// (after reading only the version byte) and [ErrMalformed] for an invalid
// header.
func ReadFlowOpen(r io.Reader) (FlowOpen, error) {
	var buf [MaxFlowOpenLen]byte
	if err := readVersion(r); err != nil {
		return FlowOpen{}, err
	}
	if err := readFull(r, buf[1:flowOpenFixedLen]); err != nil {
		return FlowOpen{}, err
	}
	kind, family, flags := Kind(buf[1]), buf[2], buf[3]
	port := binary.BigEndian.Uint16(buf[4:6])

	// Reject a bad fixed header before waiting for address bytes that a
	// broken peer may never send.
	var addrLen int
	switch family {
	case familyIPv4:
		addrLen = 4
	case familyIPv6:
		addrLen = 16
	default:
		return FlowOpen{}, fmt.Errorf("%w: unknown address family %d", ErrMalformed, family)
	}
	if flags != 0 {
		return FlowOpen{}, fmt.Errorf("%w: reserved flags %#04x set", ErrMalformed, flags)
	}
	if err := checkKindPort(kind, port); err != nil {
		return FlowOpen{}, err
	}
	if err := readFull(r, buf[flowOpenFixedLen:flowOpenFixedLen+addrLen]); err != nil {
		return FlowOpen{}, err
	}
	addr, _ := netip.AddrFromSlice(buf[flowOpenFixedLen : flowOpenFixedLen+addrLen])
	if err := checkAddr(addr); err != nil {
		return FlowOpen{}, err
	}
	return FlowOpen{Kind: kind, Dst: netip.AddrPortFrom(addr, port)}, nil
}

// Status is the agent's answer to a FlowOpen.
type Status uint8

const (
	// StatusOK means the agent opened the flow. Payload follows.
	StatusOK Status = 0
	// StatusRefused means the destination refused the connection.
	StatusRefused Status = 1
	// StatusUnreachable means the agent has no route to the destination.
	StatusUnreachable Status = 2
	// StatusTimeout means the destination did not answer in time.
	StatusTimeout Status = 3
	// StatusPolicy means the agent's policy forbids the destination, such as
	// a private address range or a blocked port.
	StatusPolicy Status = 4
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusRefused:
		return "REFUSED"
	case StatusUnreachable:
		return "UNREACHABLE"
	case StatusTimeout:
		return "TIMEOUT"
	case StatusPolicy:
		return "POLICY"
	default:
		return fmt.Sprintf("Status(%d)", uint8(s))
	}
}

func (s Status) valid() bool { return s <= StatusPolicy }

// FlowResultLen is the length of a FlowResult on the wire.
const FlowResultLen = 2

// FlowResult is the agent's answer to a FlowOpen and the first thing it
// writes on the flow stream.
//
// On the wire it is:
//
//	version u8 | status u8
type FlowResult struct {
	Status Status
}

// AppendBinary appends the wire encoding of r to b. It implements
// [encoding.BinaryAppender].
func (r FlowResult) AppendBinary(b []byte) ([]byte, error) {
	if !r.Status.valid() {
		return b, fmt.Errorf("%w: unknown flow status %d", ErrMalformed, r.Status)
	}
	return append(b, Version, byte(r.Status)), nil
}

// WriteFlowResult writes res to w in a single Write call.
func WriteFlowResult(w io.Writer, res FlowResult) error {
	var buf [FlowResultLen]byte
	b, err := res.AppendBinary(buf[:0])
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadFlowResult reads one FlowResult from r and nothing more. Its errors
// follow [ReadFlowOpen].
func ReadFlowResult(r io.Reader) (FlowResult, error) {
	if err := readVersion(r); err != nil {
		return FlowResult{}, err
	}
	var b [1]byte
	if err := readFull(r, b[:]); err != nil {
		return FlowResult{}, err
	}
	res := FlowResult{Status: Status(b[0])}
	if !res.Status.valid() {
		return FlowResult{}, fmt.Errorf("%w: unknown flow status %d", ErrMalformed, b[0])
	}
	return res, nil
}

// readVersion reads the version byte that starts every header. It returns
// io.EOF if r is already at its end.
func readVersion(r io.Reader) error {
	var v [1]byte
	if _, err := io.ReadFull(r, v[:]); err != nil {
		return err
	}
	if v[0] != Version {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, v[0])
	}
	return nil
}

// readFull is io.ReadFull for the rest of a message whose first byte was
// already read, so running out of input is always io.ErrUnexpectedEOF.
func readFull(r io.Reader, b []byte) error {
	_, err := io.ReadFull(r, b)
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
