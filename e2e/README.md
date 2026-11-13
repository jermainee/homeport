# e2e

Homeport's cross-folder end-to-end tests. They prove the data plane end to end, with the real gateway and the real sharing agent, all in one process:

```
WireGuard client ──WireGuard──▶ gateway ──QUIC──▶ agent ──socket──▶ origin
 (wireguard-go      (gateway/service,      (agent/, e2e      (httptest + UDP
  netstack, counting  run from a fake        build tag)        echo, loopback)
  tun)                control API)
```

- **Origin:** an `httptest` server and a UDP echo on loopback.
- **Control API:** [`internal/fakeapi`](internal/fakeapi), an in-memory API for the `/v1/gateway/*` contract in [`contracts/`](../contracts/control-api.openapi.yaml). It holds one verified member with peer `10.77.0.2` and one line owned by another member, with the agent's SPKI. It checks every request against the contract (fields, formats, body limits, idempotency, usage sequences, lease returns only after the final event) and fails the test on any violation. Its own tests check its answers against [`contracts/examples/gateway`](../contracts/examples/gateway).
- **Gateway:** run through [`gateway/service`](../gateway/service), exactly as `cmd/homeport-gateway` runs it: it announces itself, syncs the config, holds leases, reports usage and line events, and shuts down in order.
- **Agent:** the real agent, built with the `e2e` tag. It learns the gateway's QUIC endpoint and pin from what the gateway announced to the API, as the app does.
- **Client:** [`internal/wgclient`](internal/wgclient), a stock WireGuard client in userspace (wireguard-go's netstack), configured from the gateway's announcement. Its tun counts every inner IP byte.

## Destinations

The client sends its flows to public-looking addresses, as a member would: TCP to `203.0.113.10:80`, UDP to `203.0.113.10:7`, and DNS for `origin.homeport.test` to the virtual resolver `10.77.0.1`. The gateway drops packets addressed to loopback, so the agent's e2e hook `RouteTestOrigin` connects those two destinations to the loopback origin. Only the agent's last step, the socket, differs from production. The agent's resolver is a fake that answers `origin.homeport.test` with `203.0.113.10`.

## What the tests check

| Test | Checks |
| --- | --- |
| `TestTransfersArriveIntactAndEveryByteIsReportedOnce` | 1 MB and 10 MB downloads and a 1 MB upload match by SHA-256. After quiescence the gateway's bytes for the peer equal the client tun's counts exactly; the usage events at the API add up to the gateway's count; the agent's payload counters fit the transfers. One session, ending with a final event, then the lease return. |
| `TestUDPAndDNSFlowsPassThroughTheAgent` | UDP echo; DNS answered by the agent's (injected) resolver; a fetch by name; exact counts. |
| `TestHomeNetworksAndMailPortsAreRefusedQuickly` | `10.0.0.1:80`, `192.168.1.1:80` and port 25 are reset within seconds, by the agent's policy (`special_range`, `blocked_port`), without a socket. |
| `TestTheSessionStopsAtTheLeaseAndClosesAfterGrace` | The API grants a 300 kB lease, then answers 402. Traffic stops at the lease plus at most one second of traffic; during grace only DNS works; the session closes after grace; the final usage event arrives, then the lease return; nothing more is metered. |
| `TestARetriedUsageReportIsCountedOnce` | The API answers one usage report with 500, or applies it and drops the answer. The retry carries the same event IDs, and the totals stay exact. |
| `TestTheAgentReconnectsAfterTheGatewayDropsIt` | The gateway restarts on the same ports and keys; the agent reconnects; traffic flows again; line events go online, offline, online, offline; usage across both runs stays exact. |
| `TestLogsNeverNameADestination` | Drives TCP, UDP, DNS and refused flows, then checks the gateway's and agent's logs hold the session metadata but no destination address, origin port, DNS name or request path. |

Every test also ends with the API's contract check and the destination check on both logs.

## Run

```sh
GOWORK=off go test -race -count=1 -tags e2e ./...
```

Without the race detector the suite takes about 20 seconds. With it, the tunnel slows to a few hundred kB/s (gVisor's TCP processors spin on atomics, which the race detector makes expensive), so the suite takes a few minutes.

The `e2e` tag only exists for tests: `cmd/homeport-agent` refuses to build with it.

CI (`.github/workflows/e2e-ci.yml`) runs on Ubuntu and macOS for changes to `e2e/`, `gateway/`, `agent/`, `relay-protocol/` or `contracts/`: `go mod verify`, `gofmt`, `go vet`, golangci-lint and the tests with the race detector.

## Not done yet

- A smoke test against the real Worker (local D1) instead of the fake API, and a manual runbook with the desktop app.
