# relay-protocol

The wire format between the Homeport edge gateway and a member's sharing agent. It is a Go module (`github.com/jermainee/homeport/relay-protocol`, package `relayprotocol`) that `gateway/` and `agent/` both import, so the two sides cannot drift apart.

The module holds:

- the flow headers (`FlowOpen`, `FlowResult`)
- UDP framing
- control messages
- application error codes
- the QUIC and TLS config builders
- SPKI pinning and Ed25519 self-signed certificates

It holds no networking logic of its own.

## Using it

There is no `go.work`. A consuming module points at this folder with a relative `replace` directive, and its CI path filter includes `relay-protocol/**`:

```
require github.com/jermainee/homeport/relay-protocol v0.0.0
replace github.com/jermainee/homeport/relay-protocol => ../relay-protocol
```

## Connection

| Setting | Value |
| --- | --- |
| Transport | QUIC version 1 over UDP. The agent dials out to the gateway, so the home network opens no inbound port. |
| ALPN | `homeport-relay/1`. A peer that does not offer it fails the handshake. |
| TLS | TLS 1.3, mutual. Each side presents a self-signed Ed25519 certificate. |
| Trust | Each side pins the other's SPKI. The gateway accepts only agent pins in its config, and the agent accepts only its gateway's pin. |
| Session resumption | Off on both sides. `VerifyPeerCertificate` does not run on resumed sessions, so a removed pin could otherwise reconnect with an old ticket. |
| Keep-alive | Every 15 s from both sides, so the NAT in front of the agent keeps its UDP mapping. |
| Idle timeout | 60 s, which is four missed keep-alives. |
| Streams the agent may open | 1 bidirectional stream (the control stream). No unidirectional streams. |
| Streams the gateway may open | 1,024 concurrent bidirectional streams (one per flow). No unidirectional streams. Each flow holds an OS socket on the agent, so the agent must allow more than 1,024 open files. |

`AgentQUICConfig`, `GatewayQUICConfig`, `AgentTLSConfig` and `GatewayTLSConfig` build these settings. `PeerPin` returns the pin of the certificate the peer presented, which is how the gateway finds the agent's device.

### Pins

A pin is the SHA-256 hash of the certificate's DER-encoded SubjectPublicKeyInfo, as in HPKP's `pin-sha256`. Its text form, also used in JSON, is standard base64 with padding (44 characters). The pin depends only on the key, so a certificate rebuilt from a stored key (`NewSelfSigned`) keeps its pin. Certificates use the RFC 5280 "no expiration" date: trust rests on the pinned key, and neither side checks validity dates.

To compute a pin from a PEM certificate with OpenSSL:

```bash
openssl x509 -in cert.pem -pubkey -noout | openssl pkey -pubin -outform DER | openssl dgst -sha256 -binary | base64
```

## Streams

- **Control stream:** the agent opens it right after the handshake. It is the agent's only stream. It carries control messages in both directions.
- **Flow streams:** the gateway opens one bidirectional stream per relayed flow: a TCP connection, a UDP flow, or a client's DNS queries.

Integers on the wire are big-endian.

### FlowOpen (gateway → agent)

The first bytes of every flow stream.

| Offset | Size | Field | Values |
| --- | --- | --- | --- |
| 0 | 1 | version | `1` |
| 1 | 1 | kind | `1` TCP, `2` UDP, `3` DNS |
| 2 | 1 | family | `4` IPv4, `6` IPv6 |
| 3 | 1 | flags | reserved, must be `0` |
| 4 | 2 | port | `1`–`65535` |
| 6 | 4 or 16 | address | IPv4 or IPv6 address, as set by family |

Example, TCP to `203.0.113.7:443`: `01 01 04 00 01 bb cb 00 71 07`.

A reader rejects the header if any of these holds:

- the version is unknown, in which case it reads nothing past the version byte, because a later version may have another layout;
- the kind or family is unknown;
- a flag is set;
- the port is 0;
- the address is an IPv4-mapped IPv6 address (`::ffff:a.b.c.d`), which must be sent as IPv4 so the agent's IPv4 deny-list applies.

For DNS, the address and port are the virtual resolver the client queried (`10.77.0.1:53`). The agent does not connect to them. It resolves each query through the system resolver, so websites see the home ISP's resolver.

### FlowResult (agent → gateway)

The agent's first bytes on the flow stream.

| Offset | Size | Field | Values |
| --- | --- | --- | --- |
| 0 | 1 | version | `1` |
| 1 | 1 | status | `0` OK, `1` REFUSED, `2` UNREACHABLE, `3` TIMEOUT, `4` POLICY |

| Status | Meaning |
| --- | --- |
| OK | The agent opened the socket. Payload follows. |
| REFUSED | The destination refused the connection. |
| UNREACHABLE | The agent has no route to the destination. |
| TIMEOUT | The destination did not answer in time. |
| POLICY | The agent's policy forbids the destination, for example a private range, the line's own public IP, or a blocked port. |

### Flow lifecycle

1. The gateway opens a stream and writes `FlowOpen`. It may write payload right after it, without waiting for the result. For TCP it has none yet, because the client's handshake completes only after OK.
2. If the header is malformed, the agent resets both directions of the stream with `PROTOCOL_ERROR`, or with `UNSUPPORTED_VERSION` for an unknown version.
3. The agent applies its policy, opens the socket and writes `FlowResult`.
   - **Not OK:** the agent closes its side of the stream after the result and discards any payload that arrived.
   - **OK, TCP:** the stream carries the raw byte stream in both directions. A FIN maps to closing the send side of the stream (`Close`), and the other side half-closes its socket. A TCP reset maps to resetting the stream with `FLOW_RESET`, and the other side resets its socket.
   - **OK, UDP:** the stream carries UDP frames in both directions, one datagram per frame.
   - **OK, DNS:** each frame from the gateway is one DNS query in DNS wire format. The agent answers each query with one frame holding the response.
4. Either side ends a flow early with `FLOW_CANCELLED`, for example when the client is gone, the lease is exhausted, or a UDP flow is idle.

### UDP frame

| Size | Field |
| --- | --- |
| 2 | length, `0`–`65535` |
| length | payload |

A zero-length frame is a valid empty datagram. A stream that ends between frames ends the flow. A stream that ends inside a frame is an error.

### Control messages

Each message is a 4-byte length (1 to 65,536) followed by a JSON object of that many bytes. The object's `type` field names the message. A reader rejects a length over 65,536 before reading the body.

| Type | Direction | Fields |
| --- | --- | --- |
| `hello` | agent → gateway, first message | `agent_version` (release, such as `"0.1.0"`), `os` (`"darwin"`, `"linux"` or `"windows"`) |
| `hello_ack` | gateway → agent | `line_id` (the line this agent serves, found from its pin), `observed_ip` (the agent's public IP as the gateway sees it; the agent refuses flows to it) |
| `stats` | agent → gateway, periodic | `flows_active`, `flows_total`, `bytes_out` (to destinations), `bytes_in` (from destinations). The counters are cumulative since the control stream opened. |

Example: the 49-byte object `{"type":"stats","flows_active":2,"bytes_in":2500}` goes on the wire after the length `00 00 00 31`.

Readers ignore unknown fields. A message of an unknown type is read in full and reported as `ErrUnknownMessage`, so the receiver can skip it and stay in sync.

`stats` exists so the agent's count can be compared with the gateway's, which is the meter of record. Like every log in Homeport, control messages carry session metadata only: never destinations, DNS names or content.

### Error codes

Used with `CloseWithError` on connections and with `CancelRead`/`CancelWrite` on streams.

| Code | Name | Use |
| --- | --- | --- |
| `0x00` | NO_ERROR | Normal close. |
| `0x01` | PROTOCOL_ERROR | The peer broke the wire format, or opened a stream it may not open. |
| `0x02` | UNSUPPORTED_VERSION | The agent does not support the FlowOpen version. |
| `0x03` | INTERNAL_ERROR | The sender failed for its own reasons. |
| `0x04` | UNAUTHORIZED | The gateway does not accept this agent, for example because the device was removed or the line is not active. |
| `0x05` | GOING_AWAY | The sender is shutting down or pausing. The agent reconnects, to another gateway if needed. |
| `0x06` | FLOW_RESET | One end of a relayed TCP connection was reset. The receiver resets its side instead of closing it with FIN. |
| `0x07` | FLOW_CANCELLED | The flow ended early for another reason. |

## Development

```bash
GOWORK=off go test -race -count=1 ./...
```

`go test -fuzz` takes one target per run:

```bash
go test -run='^$' -fuzz='^FuzzReadFlowOpen$' -fuzztime=20s .
go test -run='^$' -fuzz='^FuzzReadUDPFrame$' -fuzztime=20s .
```

CI (`.github/workflows/relay-protocol-ci.yml`) runs `go mod verify`, `gofmt`, `go vet`, golangci-lint, the tests with the race detector, and every fuzz target for 20 s.
