# e2e

Homeport's cross-folder end-to-end tests and dev tools:

- **The relay suite** proves the data plane end to end, with the real gateway and the real sharing agent in one process, run from a fake control API.
- **The full-stack smoke test** runs the real control API (the Worker in `wrangler dev`, local D1) and the gateway binary, and checks the money path: bytes through the tunnel, then usage events, then ledger entries. See [Full-stack smoke test](#full-stack-smoke-test).
- **Dev tools** for manual runs: `wgcurl`, a WireGuard client in userspace, and `member`, which stands in for the desktop app's Use mode. See [Manual run](#manual-run).

## Relay suite

All in one process:

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

### Destinations

The client sends its flows to public-looking addresses, as a member would: TCP to `203.0.113.10:80`, UDP to `203.0.113.10:7`, and DNS for `origin.homeport.test` to the virtual resolver `10.77.0.1`. The gateway drops packets addressed to loopback, so the agent's e2e hook `RouteTestOrigin` connects those two destinations to the loopback origin. Only the agent's last step, the socket, differs from production. The agent's resolver is a fake that answers `origin.homeport.test` with `203.0.113.10`.

### What the tests check

| Test | Checks |
| --- | --- |
| `TestTransfersArriveIntactAndEveryByteIsReportedOnce` | 1 MB and 10 MB downloads and a 1 MB upload match by SHA-256. After quiescence the gateway's bytes for the peer equal the client tun's counts exactly; the usage events at the API add up to the gateway's count; the agent's payload counters fit the transfers. One session, ending with a final event, then the lease return. |
| `TestUDPAndDNSFlowsPassThroughTheAgent` | UDP echo; DNS answered by the agent's (injected) resolver; a fetch by name; exact counts. |
| `TestHomeNetworksAndMailPortsAreRefusedQuickly` | `10.0.0.1:80`, `192.168.1.1:80` and port 25 are reset within seconds, by the agent's policy (`special_range`, `blocked_port`) before it connects. |
| `TestTheSessionStopsAtTheLeaseAndClosesAfterGrace` | The API grants a 300 kB lease, then answers 402. Traffic stops at the lease plus at most two seconds of traffic (the gateway checks leases once a second); during grace only DNS works; the session closes after grace; the final usage event arrives, then the lease return; nothing more is metered. |
| `TestARetriedUsageReportIsCountedOnce` | The API answers one usage report with 500, or applies it and drops the answer. The retry carries the same event IDs, and the totals stay exact. |
| `TestTheAgentReconnectsAfterTheGatewayDropsIt` | The gateway restarts on the same ports and keys; the agent reconnects; traffic flows again; line events go online, offline, online, offline; usage across both runs stays exact. |
| `TestLogsNeverNameADestination` | Drives TCP, UDP, DNS and refused flows, then checks the gateway's and agent's logs hold the session metadata but no destination address, origin port, DNS name or request path. |

Every test also ends with the API's contract check and the destination check on both logs.

### Run

```sh
GOWORK=off go test -race -count=1 -tags e2e ./...
```

Without the race detector the suite takes about 20 seconds. With it, the tunnel slows to a few hundred kB/s (gVisor's TCP processors spin on atomics, which the race detector makes expensive), so the suite takes a few minutes.

The `e2e` tag only exists for tests: `cmd/homeport-agent` refuses to build with it.

CI (`.github/workflows/e2e-ci.yml`, job `test`) runs on Ubuntu and macOS for changes to `e2e/`, `gateway/`, `agent/`, `relay-protocol/`, `contracts/` or `api/`: `go mod verify`, `gofmt`, `go vet`, golangci-lint (with the `e2e` and `smoke` tags) and the tests with the race detector.

## Full-stack smoke test

`TestFullStackSmoke` (`smoke_test.go`, build tags `e2e` and `smoke`) runs Homeport as members meet it, all on one machine:

```
member B's WireGuard ──▶ gateway binary ──QUIC──▶ member A's agent ──▶ origin
 client (wgclient,        (cmd/homeport-gateway,    (agent/, e2e tag)    (loopback)
 the API's .conf)          own process)
                               │ config, leases, usage, line events
                               ▼
                         control API: wrangler dev, fresh local D1 (own process)
```

1. **API.** `npm run seed:dev -- --persist-to <tmp>` applies the migrations to an empty local D1 and adds a dev gateway; `wrangler dev` serves that database with `ENVIRONMENT=dev`, so the dev routes are on.
2. **Gateway.** `go build` of `cmd/homeport-gateway`, started with the seeded token. It announces itself with `PUT /v1/gateway/self`.
3. **Members,** through the member endpoints and the dev routes, as `memberapi` calls them: A and B sign in (`/dev/login-requests/{id}/confirm`), both are marked verified (`/dev/members/{id}/identity/verify`), B gets 1,000,000 credits (`/dev/members/{id}/credits`). A registers an agent key, creates a line and turns sharing on; B creates a WireGuard peer on it and gets its `.conf`.
4. **Restart.** The gateway learns new lines and peers at its next config poll, every 30 s. The test restarts it (same state dir and ports), so its first poll serves both.
5. **Agent and client.** A's agent dials the gateway the API named for the line, with the pin it named. B's client runs the API's `.conf` with B's own key, parsed by `wgclient.ParseConfig`.
6. **Traffic.** B downloads 3 MB and uploads 1 MB through A's line. Then the `wgcurl` binary fetches 1 MB more with the same `.conf` and reports the bytes its tunnel carried. Then the gateway gets SIGTERM, which ends the session with a final usage event and returns the lease.

The agent and the client run in the test's process. The agent needs the `e2e` tag's `RouteTestOrigin` to reach the loopback origin through a real gateway (see [Destinations](#destinations)), and `cmd/homeport-agent` refuses that tag on purpose. The client's tun counts every inner byte, which the checks compare with what the API recorded. The [manual run](#manual-run) below uses the agent CLI and a public website instead.

| Check | How |
| --- | --- |
| B's transfers arrive intact | SHA-256 of the download, of the upload at the origin, and of `wgcurl`'s body |
| The gateway reported every byte | The API's `usage_sessions` for B's peer hold exactly the bytes B's tun counted plus those `wgcurl` reported |
| B paid 10 credits per MB | `GET /v1/me`: B's `purchased` is 1,000,000 − ⌊bytes × 10 / 10^6⌋ |
| A earned 5 credits per MB | `GET /v1/me`: A's `earned_pending` is ⌊bytes × 5 / 10^6⌋ |
| The platform kept the difference | `platform:platform_revenue` |
| Every ledger transaction sums to zero | No transaction with a nonzero sum or fewer than two entries; every cached balance is the sum of its entries |
| The gateway returned its leases | No open lease or credit reservation after SIGTERM |
| No self-dealing | B shares a line of its own; a peer on it gets `403 line_not_allowed` |
| No log names a destination | The gateway's, the agent's and `wrangler dev`'s logs hold no origin address, port or request path |

The SQL checks run in one `wrangler d1 execute --local --json` on the test's database.

### Run

```sh
cd api && npm ci && cd ../e2e
GOWORK=off go test -count=1 -tags e2e,smoke -run TestFullStackSmoke -v .
```

It needs Node.js 26 and Go, and takes about 10 seconds once `wrangler dev` is up. It runs on Linux and macOS: it stops `wrangler dev` and the gateway through their process groups.

CI runs it in the `smoke` job of `e2e-ci.yml` on Ubuntu, with `go vet` for the `smoke` tag.

## Dev tools

Both are for local runs against `wrangler dev`, and both build without the `e2e` tag.

- **`cmd/wgcurl`** fetches a URL through a WireGuard tunnel in userspace (wireguard-go's netstack), so it needs no root and changes no routes; it can run on the sharing Mac itself. It reads a wg-quick `.conf` with the private key filled in, resolves names through the tunnel's DNS (the agent's resolver), writes the body to stdout or `-o`, and reports the status, the body's SHA-256 and the inner bytes the tunnel carried. The gateway's meter for the peer must match those bytes.
- **`cmd/member`** does what the app's Use mode will do (P17): `signin EMAIL [-dev]`, `me`, `identity` (Stripe Identity page), `checkout` (Stripe Checkout page), `lines`, `peer LINE_ID` (creates the key pair locally and prints the `.conf` with the private key), and `share KEY_FILE` (registers the agent CLI's key, creates the line, turns sharing on and prints the agent's `HOMEPORT_GATEWAY` and `HOMEPORT_GATEWAY_PIN`). `verify` and `credits N` use the dev routes. Its session file has the app's `session.json` format, so `-session <profile>/session.json` acts as a desktop-app profile's member. `signin` never replaces an existing session file, so it cannot sign a profile out; the file it writes is readable by the user only.

`internal/memberapi` is the client both the tools and the smoke test use. The smoke test runs `wgcurl`; `cmd/member` has its own tests against a stub API.

## Manual run

A local run of the whole product on one Mac, with real Stripe test mode: two desktop-app profiles, the API, the gateway and the iPhone WireGuard app or `wgcurl`. Paths below put keys, sessions and configs into `.dev-state/` at the repo root, which git ignores. Run each long-running command in its own terminal, from the repo root.

**You need:**
- Node.js 26 (CI's version; Wrangler runs on 22.12 or later), Go, and the Wails CLI (see [`desktop-app/README.md`](../desktop-app/README.md));
- the [Stripe CLI](https://docs.stripe.com/stripe-cli), logged in to the sandbox (`stripe login`, then pick the sandbox);
- for the iPhone, the WireGuard app.

The desktop app shows the QR code itself. `qrencode` (`brew install qrencode`) is needed only on the `member` tool's path.

**In the Stripe sandbox, first:**
- create two restricted keys (Dashboard → Developers → API keys):
  - one with write access to Checkout Sessions and Identity Verification Sessions, for `STRIPE_SECRET_KEY`;
  - one with read access to Identity verification sessions and reports, including their sensitive verified outputs, for `STRIPE_IDENTITY_RESTRICTED_KEY`;
- check under Identity that the sandbox may create verification sessions;
- for tax, the head office and the registrations are set up in Tax (see [`api/README.md`](../api/README.md)).

### 1. API and Stripe

```sh
cd api
npm ci
[ -f .dev.vars ] || cp .dev.vars.example .dev.vars  # then add the two restricted keys and any long random IDENTITY_HASH_KEY
stripe listen --forward-to localhost:8787/v1/stripe/webhook
```

Put the `whsec_…` that `stripe listen` prints into `.dev.vars` as `STRIPE_WEBHOOK_SECRET`, and keep `stripe listen` running. Then:

```sh
npm run seed:dev  # prints HOMEPORT_GATEWAY_TOKEN=hpgw_…
npm run dev       # wrangler dev on localhost:8787, local D1; sign-in links appear in this log
```

### 2. Gateway

```sh
cd gateway
HOMEPORT_GATEWAY_TOKEN=hpgw_… go run ./cmd/homeport-gateway \
  --api http://localhost:8787 \
  --wg-endpoint "$(ipconfig getifaddr en0):51820" \
  --state-dir ../.dev-state/gateway
```

`--wg-endpoint` goes into every new peer's `.conf`. For the iPhone, it must be the Mac's address on the Wi-Fi the phone uses; for `wgcurl` alone, `127.0.0.1:51820` works. Agents dial the same host on port 4433. Ctrl-C stops the gateway in order: final usage events, lease returns, lines offline.

The gateway polls its config every 30 s. A new line or peer works from the next poll; restarting the gateway polls at once.

### 3. Member A shares (profile A)

```sh
cd desktop-app
wails3 build
HOMEPORT_PROFILE=../.dev-state/profile-a ./bin/homeport
```

1. Sign in as `a@example.com`: open the link from the `npm run dev` log and confirm the match code.
2. On the Share tab, click "Verify identity". Stripe's test page opens in the browser; choose its verified option. The `identity.verification_session.verified` webhook arrives through `stripe listen`, and the app turns Share on without a restart.

   Without Stripe keys, the dev route does the same:

   ```sh
   cd e2e && go run ./cmd/member -session ../.dev-state/profile-a/session.json verify
   ```

3. Turn Share on. The agent connects at the gateway's next config poll; until then it retries with a growing pause, so this takes up to about a minute.

Without the desktop app (on Linux, say), the agent CLI shares the line instead:

```sh
cd e2e
go run ./cmd/member -session ../.dev-state/a.json signin a@example.com  # or -dev to skip the link
go run ./cmd/member -session ../.dev-state/a.json identity               # or verify
(cd ../agent && go run ./cmd/homeport-agent -print-pin -key ../.dev-state/a-agent.key)
go run ./cmd/member -session ../.dev-state/a.json share ../.dev-state/a-agent.key > ../.dev-state/a-agent.env
cd ../agent && env $(grep -v '^#' ../.dev-state/a-agent.env) go run ./cmd/homeport-agent -key ../.dev-state/a-agent.key -v
```

### 4. Member B uses A's line (profile B)

Run profile B from the same build:

```sh
HOMEPORT_PROFILE=../.dev-state/profile-b ./bin/homeport  # in desktop-app/
```

Then, in profile B's window:
1. Sign in as `b@example.com`.
2. On the Use tab, click "Verify identity" and choose Stripe's verified test option.
3. Click "Buy credits" and pay on Stripe Checkout with `4242 4242 4242 4242`, any future date and any CVC. Checkout asks for an address, which sets the tax: a German address adds 19% VAT. Once `checkout.session.completed` arrives through `stripe listen`, the balance shows 1,000,000 credits.
4. Pick A's line.
5. Add a device. The app shows its WireGuard config **once**: as a QR code for the iPhone, and through "Save .conf file" for a Mac, saved as `.dev-state/b.conf` for the next step.

The same steps work from the terminal on B's app session:

```sh
cd e2e
member() { go run ./cmd/member -session ../.dev-state/profile-b/session.json "$@"; }
member identity   # or: member verify, through the dev route
member checkout
member me
member lines
member peer <A's line_id> > ../.dev-state/b.conf
```

Only lines with a known country are listed, in the app and in `member lines`. `wrangler dev` takes the country from Cloudflare's view of the request, so offline there is none. `member peer` still works offline, with the line id from the gateway's log or the database.

### 5. Traffic

- **iPhone:** in the WireGuard app, add a tunnel with "Create from QR code", scan the QR code in profile B's window, turn the tunnel on and browse. On the `member` tool's path, `qrencode -t ansiutf8 < ../.dev-state/b.conf` prints the QR code in the terminal instead.
- **Mac:** `go run ./cmd/wgcurl -conf ../.dev-state/b.conf -o /dev/null 'https://speed.cloudflare.com/__down?bytes=5000000'` prints the status, the SHA-256 and the bytes the tunnel carried.

Either way the traffic leaves through A's agent, and the gateway logs `session started` for B's peer.

### 6. Check the ledger

Usage reaches the API every 30 s, and in full when the session ends: after 2 minutes without traffic, or at once when the gateway stops (Ctrl-C). Then, in `api/`:

```sh
# Bytes per session and the credits they cost
npx wrangler d1 execute homeport --local --command "SELECT m.email, s.bytes_up + s.bytes_down AS bytes, (s.bytes_up + s.bytes_down) * 10 / 1000000 AS credits FROM usage_sessions s JOIN members m ON m.id = s.member_id"

# Every account that moved
npx wrangler d1 execute homeport --local --command "SELECT COALESCE(m.email, a.id) AS account, a.kind, a.balance FROM ledger_accounts a LEFT JOIN members m ON m.id = a.member_id WHERE a.balance <> 0 ORDER BY account, a.kind"

# Transactions that do not sum to zero: must return no rows
npx wrangler d1 execute homeport --local --command "SELECT t.id, t.kind, COALESCE(SUM(e.amount), 0) AS total, COUNT(e.id) AS entries FROM ledger_transactions t LEFT JOIN ledger_entries e ON e.transaction_id = t.id GROUP BY t.id HAVING total <> 0 OR entries < 2"
```

B's `purchased` dropped by ⌊bytes × 10 / 10^6⌋ over all of B's sessions on the peer, A's `earned_pending` rose by ⌊bytes × 5 / 10^6⌋, and `platform:platform_revenue` holds the difference. `member me` shows the same balances.

### 7. Negative checks

- **No self-dealing:** turn Share on in profile B. `member lines` does not list B's own line, and `member peer <B's line_id>` answers `403 line_not_allowed`.
- **A stops sharing:** quit profile A's app or turn Share off. B's flows stop, and `wgcurl` fails until A shares again.
- **No credits:** sign in a third member, verify them and create a peer on A's line without buying credits. `wgcurl https://example.com/` fails, while `wgcurl https://checkout.stripe.com/` still answers, because Stripe Checkout is a top-up host. After 5 minutes of grace the gateway closes the session.

## Not done yet

- The smoke test's agent runs in-process with the `e2e` tag; the agent CLI is covered only by the manual run.
- A new line or peer waits for the gateway's next config poll (up to 30 s), and a new agent backs off meanwhile, so turning Share on takes up to about a minute. A config refresh when an unknown agent key connects would make it immediate.
