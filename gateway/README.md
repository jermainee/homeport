# gateway

Homeport's edge gateway. It terminates members' WireGuard tunnels in userspace, counts every inner IP byte per peer, and hands each TCP or UDP flow to a `FlowDialer`.

## How it works

- One wireguard-go device serves every peer. Each peer may only send from its own tunnel addresses (`/32` and `/128`), so a packet's source address identifies the peer.
- `internal/wgstack` is a gVisor network stack in promiscuous mode with spoofing on. It accepts flows to any destination through gVisor's TCP and UDP forwarders.
- A TCP handshake with the client completes only after the `FlowDialer` has connected the far side. A refused flow reaches the client as a reset.
- The meter counts inner IP packets at the boundary between WireGuard and the stack, in both directions. These are the bytes members are charged for (10 credits per MB, see `AGENTS.md`). Packets from or to unknown tunnel addresses are dropped uncounted.
- Down bytes are counted when the stack hands a packet to WireGuard. If WireGuard then drops it, for example a TCP retransmission to a peer that just went offline, it still counts.
- Each tunnel address may have at most `MaxPendingFlowsPerPeer` flows (default 64) waiting on the dialer. More are reset, so one member cannot block new connections for everyone.
- Re-adding a peer keeps its usage. `RemovePeer` returns the peer's final usage so it can still be billed. `Close` ends every flow.

Still to come: the QUIC relay to sharing agents (P8), the control-plane client for config, leases and usage (P9), and short idle timeouts for DNS flows.

## Develop

```sh
GOWORK=off go test -race -count=1 ./...
```

The tests run a real userspace WireGuard client (wireguard-go's netstack) against the gateway.
