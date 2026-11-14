# Homeport

Homeport (working title) is a residential line marketplace. Members share their home internet line and use other members' lines over WireGuard or SOCKS5, and pay in credits through Stripe.

Everything Homeport installs on a member's device supports the latest releases of Linux, macOS and Windows. Older OS releases are out of scope.

## Repository layout

Everything is in one git repository. Each piece of software lives in its own top-level folder that works like a standalone repo, with its own manifest, lockfile, tests, README and GitHub workflows. Code that two or more folders need moves into its own folder, which follows the same rules.

| Folder | What it is | How it ships |
| --- | --- | --- |
| `api/` | Control plane: accounts, identity status, credits ledger, leases, lines, WireGuard peers, Stripe webhooks | Deploy workflow to Cloudflare Workers + D1 |
| `gateway/` | Edge gateway: terminates WireGuard, relays flows to agents over QUIC, meters bytes per peer | Deploy target not decided yet |
| `agent/` | Sharing agent: Go library and CLI that opens real sockets on the member's line | Inside `desktop-app/`; Docker image later |
| `relay-protocol/` | Go wire format shared by `gateway/` and `agent/` | CI only |
| `desktop-app/` | Share and Use app for Linux, macOS and Windows, built with Wails v3 | Release workflow attaches the installers to a GitHub Release |
| `contracts/` | OpenAPI spec and examples for the API that `gateway/` and `desktop-app/` call | CI only |
| `e2e/` | Cross-folder end-to-end tests and dev tools | CI only |
| `landing-page/` | Public website (later) | Deploy workflow to Cloudflare Pages |

New web parts run on Cloudflare (Pages, Workers, D1) and ship through a deploy workflow. New installable apps ship through a release workflow. When you create a folder, add its row to the table above, or remove "(planned)" from it.

## Git workflow

1. Branch from `main` as `feature/<slug>` or `fix/<slug>`. The slug names the issue in 2–4 kebab-case words: `feature/credit-ledger`, `fix/lease-renewal`.
2. Write commit subjects in the imperative: `Add credit ledger`, `Fix lease renewal at 80%`.
3. Every change reaches `main` through a pull request, including changes to this file.
4. Before merging a PR that changes code or config, run a code review on it. Fix every finding, or reply in the PR explaining why it stays. A PR that changes only Markdown (`.md`) files skips the review.
5. Deployments and releases run only in GitHub Actions, triggered by the merge into `main`.

## Building

Work test-first. Start with a failing test (red), make it pass, then refactor. A bug fix starts with a test that reproduces the bug.

## CI/CD conventions

- Workflows live in `.github/workflows/` and are named `<folder>-<purpose>.yml`, for example `landing-page-ci.yml`, `landing-page-deploy.yml` and `desktop-app-release.yml`.
- Each workflow runs only for changes under its own folder (`<folder>/**`), the folders it depends on, and its own workflow file, so a change ships only what it touches.
- **Go folders** link each other with relative `replace` directives (for example `gateway/` → `../relay-protocol`). A folder's workflow also watches the folders it depends on. There is no committed `go.work`, and CI runs with `GOWORK=off`.
- `<folder>-ci.yml` runs lint, type checks and tests on every PR.
- **Apps:** on merge into `main`, the release workflow builds each platform the app supports so far on its native runner: an AppImage for Linux, a signed and notarized `.dmg` for macOS and an installer for Windows. macOS comes first. The builds are published as a GitHub Release tagged `<folder>-v<version>`. The version comes from the app's manifest, so any PR that changes an app also bumps its version.
- **Landing page:** on merge into `main`, `landing-page-deploy.yml` deploys to Cloudflare Pages with Wrangler.
- **Workers and D1:** schema changes ship as D1 migration files, and the deploy workflow applies them.
- **Secrets:** Cloudflare tokens, Stripe keys and Apple signing certificates are stored in GitHub Actions secrets and Cloudflare secrets. The repo holds only example files such as `.dev.vars.example`.

## Product rules the code enforces

The full product concept, with the reasoning behind these rules, is in [docs/concept.md](docs/concept.md). When a change alters a rule, update that file and this list in the same PR.

These are the current values. Keep them in one pricing module, and have all other code read them from there.

- **Identity:** every member passes Stripe Identity (ID document and selfie) before Share or Use turns on. A line, a WireGuard config and a listing each require a verified identity.
- **Credits:** credits are integers. 1 MB = 10^6 bytes, counting upload plus download.
  - Using costs 10 credits per MB, and sharing earns 5.
  - An exclusive line costs its user 10,000 credits a day and earns its owner 5,000.
  - 1,000,000 credits sell for €40 net.
- **Ledger:** double-entry and append-only. A correction is made with a reversing entry. Euros appear only at purchase and cash-out, in integer cents.
- **Purchased vs. earned credits:** purchased credits are spent first and are never paid out. Earned credits can be cashed out after a 30-day holdback.
- **Metering:** the gateway is the meter of record and counts usage per WireGuard peer or SOCKS5 login. That way, a stock WireGuard client on a travel router is metered the same as the app.
- **No self-dealing:** a member's traffic always exits through someone else's line. It never exits through their own line, or through a line linked to them by household, device, card or Connect account.
- **Isolation:** a member's flows never reach another member's device, the gateway (except DNS on the virtual resolver) or a private or other special-purpose range. The gateway refuses them before any agent sees them, and the agent refuses them again, together with the seller's own IP and home networks.
- **Logging:** logs hold only session metadata (member, line, start, end, bytes). Destinations and content are never logged.

## Language

Write code, comments, commits, PRs and docs in English.
