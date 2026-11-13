# gateway

Homeport's edge gateway. It terminates members' WireGuard tunnels in userspace, counts every inner IP byte per peer, and hands each TCP or UDP flow to a `FlowDialer`.

## How it works

- One wireguard-go device serves every peer. Each peer may only send from its own tunnel addresses (`/32` and `/128`), so a packet's source address identifies the peer.
- `internal/wgstack` is a gVisor network stack in promiscuous mode with spoofing on. It accepts flows to any destination through gVisor's TCP and UDP forwarders.
- A TCP handshake with the client completes only after the `FlowDialer` has connected the far side. A refused flow reaches the client as a reset.
- The meter counts inner IP packets at the boundary between WireGuard and the stack, in both directions. These are the bytes members are charged for (10 credits per MB, see `AGENTS.md`). Packets from or to unknown tunnel addresses are dropped uncounted.

Still to come: the QUIC relay to sharing agents (P8) and the control-plane client for config, leases and usage (P9).

## Develop

```sh
GOWORK=off go test -race -count=1 ./...
```

The tests run a real userspace WireGuard client (wireguard-go's netstack) against the gateway.
