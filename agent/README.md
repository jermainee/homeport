# agent

Homeport's sharing agent. It runs on a member's home machine, dials out to an edge gateway and opens a real socket on the member's line for every flow the gateway relays to it, so websites see a genuine home connection. It is a Go library (`github.com/jermainee/homeport/agent`), which the desktop app embeds, and a CLI for development and the later Docker image.

The wire format is [`relay-protocol/`](../relay-protocol/README.md).

## How it works

- **Connection:** the agent dials the gateway over QUIC with mutual TLS. It presents its own self-signed Ed25519 certificate and accepts only the gateway pin it was given. The home network opens no inbound port.
- **Control stream:** the agent sends `hello` (version and OS) and waits for `hello_ack`, which names the line and the agent's public IP as the gateway sees it. It then sends `stats` every 30 s: its own count of flows and payload bytes since the control stream opened. The gateway stays the meter of record.
- **Flows:** the gateway opens one stream per flow, and the agent answers each `FlowOpen` with a `FlowResult`:
  - **TCP:** the agent connects and relays the byte stream. A FIN becomes a half-close on the other side, and a reset (`FLOW_RESET`) becomes a reset.
  - **UDP:** the agent connects a UDP socket and relays one frame per datagram. A UDP flow idle for 2 minutes ends with `FLOW_CANCELLED`.
  - **DNS:** each frame is a query. The agent answers A and AAAA queries, answers HTTPS and SVCB queries with no records, and returns NOTIMP for other types. It never dials the virtual resolver address in the header. See "DNS over HTTPS" below.
  - **Gateway close:** STOP_SENDING from the gateway on a TCP flow means the client is gone. The agent stops reading the socket but still delivers the gateway's remaining bytes up to its FIN; with `FLOW_RESET` it resets the socket.
  - A malformed header resets the stream with `PROTOCOL_ERROR`, or `UNSUPPORTED_VERSION` for an unknown version.
- **Reconnects:** after a failure the agent retries after 1 s, doubling up to 1 minute, with jitter. After `UNAUTHORIZED` (for example, a removed device) it waits the full minute. When the gateway refuses its certificate in the handshake (`bad_certificate`), its config may not list the line yet, as right after the member turned Share on. The refusal makes the gateway fetch its config at once (at most every 2 s), so the agent retries after 1 s (jittered to 0.5 to 1 s) up to five times in a row before the usual backoff takes over.
- **Open files:** each flow holds a socket, so the agent raises its open-file limit to 2,048 where the hard limit allows it.

## Home network protection

The policy runs in the dialer's `Control` hook, on the address the socket is about to connect to. The agent refuses a flow with `POLICY` if:

- the destination is in a private, local or special-purpose range (`relayprotocol.SpecialPurpose`, which the gateway applies too): `0/8`, `10/8`, `100.64/10`, `127/8`, `169.254/16`, `172.16/12`, `192.0.0/24`, `192.168/16`, `198.18/15`, the documentation ranges, multicast, `240/4`, `::/96`, `fc00::/7`, `fe80::/10`, `fec0::/10`, IPv6 multicast and the other non-global IPv6 ranges. NAT64 (`64:ff9b::/96`) and 6to4 (`2002::/16`) addresses are checked by the IPv4 address inside them.
- the destination is the line's own public IP (`hello_ack`'s `observed_ip`);
- the destination is on a network of one of the machine's own interfaces, which keeps a LAN with public IPv4 addresses out of reach. Each global IPv6 address (`2000::/3`) counts as the `/56` around it, the prefix most ISPs delegate to a home, so the home's other `/64`s (guest, IoT) and an address DHCPv6 reports as a `/128` are covered too;
- the port is 25, 465 or 587 (mail).

DNS answers get the same address rules: an answer that points into the home network is dropped. Names that exist only inside a network (`.local`, `localhost`, `home.arpa`, `.internal`, `.lan`, `.home`, `.localdomain`, `fritz.box` and single-label names) get NXDOMAIN without a lookup. A lookup would send mDNS or router queries into the home network, and its timing would reveal which devices exist there.

The policy reads the machine's interface networks again at every connection, so a laptop that moved to another network protects the new one from the first flow.

Builds with the `e2e` build tag have two test hooks for the end-to-end tests in `e2e/`:

- `AllowTestOrigin` lets the agent dial a loopback test server.
- `RouteTestOrigin` sends flows for a public-looking destination, such as `11.22.33.44:80`, to a loopback test server. A real gateway drops packets addressed to loopback, so tests that run through it need this. A route covers TCP and UDP alike and skips the policy for its own port; DNS answers may name the routed address, and its other ports get the normal policy.

`cmd/homeport-agent` does not compile with that tag, and CI checks that. The tag guard covers this command only: a program that embeds the library, such as the desktop app, must refuse the tag in its own release build too.

## DNS over HTTPS

The agent resolves names over DNS over HTTPS (RFC 8484) through the line: Cloudflare (`cloudflare-dns.com` at `1.1.1.1`, `1.0.0.1` and their IPv6 addresses), then Quad9 (`dns.quad9.net` at `9.9.9.9`, `149.112.112.112` and their IPv6 addresses). It dials those addresses directly, so finding the resolver needs no DNS, and uses one HTTP/2 connection per resolver for all queries.

- **Trust:** the resolver's certificate is verified against the Mozilla roots built into the agent (`golang.org/x/crypto/x509roots/fallback/bundle`), never the machine's trust store. A root installed on the machine, by the member or an intercepting proxy, does not get answers through. Bump that module to keep the roots current.
- **Privacy:** queries use ID 0 and are padded to a multiple of 128 bytes (RFC 8467), so the size of the encrypted request does not give the name away.
- **Failover:** a resolver's addresses are dialed Happy Eyeballs style: the next starts after 250 ms, or at once when one fails, so a lost SYN costs little. Each attempt at one resolver gets 2.5 s, enough for a new connection over a satellite line. A resolver that fails or does not answer is skipped for the next, and later lookups start at the one that answered last.
- **No fallback:** if neither resolver answers, the query gets SERVFAIL. The agent never falls back to the system resolver: whoever blocks DNS over HTTPS could then forge answers again.
- A name error, or an answer without records of the asked family, gets an empty NOERROR answer.

## Pausing

- `Pause()` closes the connection with `GOING_AWAY`, which ends every flow, and the agent stays away until `Resume()`.
- **Another VPN:** while another VPN carries the default route, sharing pauses the same way, so shared traffic never leaves through someone else's tunnel. On macOS, `internal/netwatch` checks every second whether traffic to the internet leaves through a `utun` or `ipsec` interface (WireGuard, OpenVPN and Network Extension VPNs, and macOS's own IKEv2 client). Linux and Windows have no check yet.
- `Stats()` reports the state, the line, why sharing is paused, the last connection error, and the flow and byte counts since the agent was created.

## Logging

Logs hold session metadata only: the gateway, the line, connection start and end, flow counts and byte counts. At debug level they add each refused flow's kind and status or policy rule. They never hold a destination, a DNS name or content. A test drives every kind of flow and checks that no destination reaches the logs.

## Use

```go
cert, _ := relayprotocol.NewSelfSigned(storedKey, "homeport-agent")
a, err := agent.New(agent.Config{
	GatewayAddr: "gateway.example:443",
	GatewayPin:  gatewayPin,
	Certificate: cert,
	Logger:      logger,
})
if err != nil {
	return err
}
go a.Run(ctx) // returns once ctx ends
```

The CLI:

```bash
go run ./cmd/homeport-agent -print-pin   # creates homeport-agent.key on first run
go run ./cmd/homeport-agent -gateway 127.0.0.1:4433 -gateway-pin <gateway pin>
```

Every flag can also come from the environment: `HOMEPORT_GATEWAY`, `HOMEPORT_GATEWAY_PIN` and `HOMEPORT_AGENT_KEY`. `-v` logs each refused flow's kind and status.

## Development

```bash
GOWORK=off go test -race -count=1 ./...
GOWORK=off go test -race -count=1 -tags e2e .
```

The tests run the agent against a fake gateway built from `relay-protocol` and quic-go, with real sockets to loopback test servers.

CI (`.github/workflows/agent-ci.yml`) runs on Ubuntu and macOS: `go mod verify`, `gofmt`, `go vet` (including the `e2e` tag and a Windows build), golangci-lint, the tests with the race detector, the `e2e` build's tests, and a check that the CLI refuses the `e2e` tag.

## Not done yet

- The route check polls once a second. Reacting to route changes as they happen would close the gap between a VPN coming up and sharing pausing.
- `hello_ack` carries one public IP, from the address family the agent used to reach the gateway. A line reached over IPv6 does not learn its public IPv4 address, so the own-IP rule covers only one family.
- Upload share and monthly caps (docs/concept.md) are not enforced yet.
