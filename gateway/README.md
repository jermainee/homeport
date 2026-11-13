# gateway

Homeport's edge gateway. It terminates members' WireGuard tunnels in userspace, counts every inner IP byte per peer, and relays each TCP or UDP flow over QUIC to the sharing agent of the line the peer uses.

## How it works

- One wireguard-go device serves every peer. Each peer may only send from its own tunnel addresses (`/32` and `/128`), so a packet's source address identifies the peer.
- `internal/wgstack` is a gVisor network stack in promiscuous mode with spoofing on. It accepts flows to any destination through gVisor's TCP and UDP forwarders.
- A TCP handshake with the client completes only after the `FlowDialer` has connected the far side. A refused flow reaches the client as a reset.
- The meter counts inner IP packets at the boundary between WireGuard and the stack, in both directions. These are the bytes members are charged for (10 credits per MB, see `AGENTS.md`). Packets from or to unknown tunnel addresses are dropped uncounted.
- Down bytes are counted when the stack hands a packet to WireGuard. If WireGuard then drops it, for example a TCP retransmission to a peer that just went offline, it still counts.
- Each tunnel address may have at most `MaxPendingFlowsPerPeer` flows (default 64) waiting on the dialer. More are reset, so one member cannot block new connections for everyone.
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

Still to come: the control-plane client for config, leases and usage (P9), and short idle timeouts for DNS flows.

## Develop

```sh
GOWORK=off go test -race -count=1 ./...
```

The tests run a real userspace WireGuard client (wireguard-go's netstack) against the gateway.
