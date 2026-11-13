# gateway

Homeport's edge gateway. It terminates members' WireGuard tunnels in userspace, counts every inner IP byte per peer, and relays each TCP or UDP flow over QUIC to the sharing agent of the line the peer uses.

## How it works

- One wireguard-go device serves every peer. Each peer may only send from its own tunnel addresses (`/32` and `/128`), so a packet's source address identifies the peer.
- `internal/wgstack` is a gVisor network stack in promiscuous mode with spoofing on. It accepts flows to any destination through gVisor's TCP and UDP forwarders.
- A TCP handshake with the client completes only after the `FlowDialer` has connected the far side. A refused flow reaches the client as a reset.
- The meter counts inner IP packets at the boundary between WireGuard and the stack, in both directions. These are the bytes members are charged for (10 credits per MB, see `AGENTS.md`). Packets from or to unknown tunnel addresses are dropped uncounted.
- Down bytes are counted when the stack hands a packet to WireGuard. If WireGuard then drops it, for example a TCP retransmission to a peer that just went offline, it still counts.
- Each tunnel address may have at most `MaxPendingFlowsPerPeer` flows (default 64) waiting on the access check and the dialer. More are reset, so one member cannot block new connections for everyone.
- `Config.Access` is asked before each new flow and answers none, full or top-up only. A top-up-only TLS flow completes the client's handshake first, so the gateway can read the server name; it is then passed on unchanged.
- Re-adding a peer keeps its usage. `RemovePeer` returns the peer's final usage so it can still be billed. `Close` ends every flow.

## Relay to agents

- Agents dial the gateway's QUIC address (`Config.QUICListen`) with mutual TLS. The wire format is in [`relay-protocol/`](../relay-protocol/README.md). The gateway accepts an agent only if its certificate pin belongs to a line (`AddLine`). That line's ID goes back to the agent in `hello_ack`, together with the public IP the gateway sees.
- A newer connection for the same line replaces the older one. `RemoveLine`, or a changed pin, disconnects the agent.
- Each peer names its member and its line (`Peer.MemberID`, `Peer.LineID`). A flow is reset at once when:
  - the peer has no line;
  - the line is offline;
  - the peer's member owns the line (no self-dealing);
  - the agent answers with anything but OK.
- Flows to port 53 on the virtual resolver (`Config.DNS`, default `10.77.0.1`) become DNS flows: the agent resolves them through the home ISP's resolver. Other UDP is relayed as UDP frames.
- `Config.Dialer` replaces the relay in tests.

## Control plane

`internal/controlplane` runs the gateway from the control API, as [`contracts/control-api.openapi.yaml`](../contracts/control-api.openapi.yaml) specifies for `/v1/gateway/*`. `service` wires it to the `Server`: it announces the gateway, runs the controller and shuts down in order. `cmd/homeport-gateway` is a thin wrapper around `service`, and the end-to-end tests in [`e2e/`](../e2e/README.md) run the gateway through it too.

- **Config:** polled every 30 s with the body's `etag` as `If-None-Match`. Each 200 replaces the previous config. A peer or line no longer listed is removed at once, open flows included. A changed peer is removed and added again, so its session ends. When applying fails, the next poll fetches the whole config again.
- **Pins:** the contract sends `device_spki_sha256` and `quic_spki_sha256` as lowercase hex. relay-protocol's `Pin` holds the same 32 bytes but prints them as base64; the client converts.
- **Sessions:** a peer's session starts with its first flow, or with any metered byte, while its line is online. It ends after 2 minutes without a byte in either direction (`IdleTimeout`), when the peer leaves the config, or when it is closed (below).
- **Leases:**
  - The first flow of a session waits for the session's first lease.
  - The next lease is requested at `renew_at_bytes`, or a minute before the current lease expires, with `supersedes_lease_id`.
  - Bytes count against the current lease until all of `granted_bytes` are used or it expires; then the session moves to the next lease. Expiry is counted on the gateway's clock: the gateway converts `expires_at` with the answer's `Date` header, so clock skew between gateway and API does not end leases early. The switch happens at the next one-second step, so a lease may end up a little over its `granted_bytes`.
- **Without a lease** (after a 402, or while the API is unreachable): the gateway resets the peer's flows and allows only DNS through the virtual resolver and TLS on port 443 whose ClientHello names a host in `topup_hostnames`. It asks again every 10 s, so a member who tops up continues without a new session. After `grace_seconds` the session closes: those of the 402, else of the current lease, else the last the API sent (300 before any).
  - The server name is read from the ClientHello in the clear. A browser using Encrypted Client Hello names only the provider's public name there (Cloudflare's is `cloudflare-ech.com`), so **top-up hosts must not offer ECH**; on Cloudflare, turn it off for their zone.
  - A retry after no answer or a 429, 5xx or 401 keeps its `request_id`. A new request after any other answer gets a new one, because the API replays stored answers.
- **Closing a session** (grace over, or 403 `peer_inactive`, at once): the peer is suspended: its flows are reset and its packets are dropped without being counted, so the final usage event holds every byte. After a minute the peer is let back, and its next byte starts a new session.
- **Usage events:** cut per session every 30 s (if bytes were counted), every 50 MB, at a lease switch, and at session end (`final`). Each event takes every byte the meter counted since the last one, so the events add up to the meter exactly. Bytes metered while no session could start (the line offline) go into the next session's first event. Batches hold up to 100 events.
  - A failed batch is sent again with the same event IDs. `rejected` events are not retried.
  - A session's leases, the current one and an unused successor, are returned only after its final event has an answer.
- **Line events:** `online` when an agent's hello registers it, with the observed IP and the hello's `agent_version` (sent as `0.0.0` when it is not a semantic version, which the API would refuse). `offline` when it disconnects or its line is removed.
- **Logs** name peers, lines, sessions, leases and events, never a destination or a server name.

## Run

```sh
HOMEPORT_GATEWAY_TOKEN=hpgw_<id>.<secret> homeport-gateway \
  --api https://api.homeport.example \
  --wg-endpoint fra1.gw.homeport.example:51820 \
  --wg-listen :51820 --quic-listen :4433 \
  --state-dir /var/lib/homeport-gateway
```

- The state dir keeps the WireGuard key (`wg.key`) and the relay certificate (`relay.crt`, `relay.key`), created on first start. Members' configs and agents' pins depend on them, so a damaged file stops the gateway instead of being replaced.
- `--quic-endpoint` defaults to the `--wg-endpoint` host with the `--quic-listen` port.
- At start the gateway sends `PUT /v1/gateway/self`, retrying while the API is unreachable (a refused token stops it). On SIGINT or SIGTERM it ends every session and refuses new ones, reports the final usage, returns the leases, disconnects its agents and reports those lines offline.

Still to come: short idle timeouts for DNS flows.

## Develop

```sh
GOWORK=off go test -race -count=1 ./...
```

The tests run a real userspace WireGuard client (wireguard-go's netstack) against the gateway.
