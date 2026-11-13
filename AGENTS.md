# Homeport

Homeport (working title) is a residential line marketplace. Members share their home internet line and use other members' lines over WireGuard or SOCKS5, and pay in credits through Stripe.

## Repository layout

Everything is in one git repository. Each piece of software lives in its own top-level folder that works like a standalone repo, with its own manifest, lockfile, tests, README and GitHub workflows. Code that two or more folders need moves into its own folder, which follows the same rules.

| Folder | What it is | How it ships |
| --- | --- | --- |
| `landing-page/` | Public website | Deploy workflow to Cloudflare Pages |
| `desktop-app/` | Share and Use app for macOS and Windows | Release workflow attaches the installers to a GitHub Release |

New web parts run on Cloudflare (Pages, Workers, D1) and ship through a deploy workflow. New installable apps ship through a release workflow. When you create a folder, add a row to the table above.

## Git workflow

1. Branch from `main` as `feature/<slug>` or `fix/<slug>`. The slug names the issue in 2–4 kebab-case words: `feature/credit-ledger`, `fix/lease-renewal`.
2. Write commit subjects in the imperative: `Add credit ledger`, `Fix lease renewal at 80%`.
3. Every change reaches `main` through a pull request, including changes to this file.
4. Before merging, run a code review on the PR. Fix every finding, or reply in the PR explaining why it stays.
5. Deployments and releases run only in GitHub Actions, triggered by the merge into `main`.

## Building

Work test-first. Start with a failing test (red), make it pass, then refactor. A bug fix starts with a test that reproduces the bug.

## CI/CD conventions

- Workflows live in `.github/workflows/` and are named `<folder>-<purpose>.yml`, for example `landing-page-ci.yml`, `landing-page-deploy.yml` and `desktop-app-release.yml`.
- Each workflow runs only for changes under its own folder (`<folder>/**`) and to its own workflow file, so a change ships only the folder it touches.
- `<folder>-ci.yml` runs lint, type checks and tests on every PR.
- **Apps:** on merge into `main`, the release workflow builds each platform on its native runner. macOS gets a signed and notarized `.dmg`, Windows an installer. Both are published as a GitHub Release tagged `<folder>-v<version>`. The version comes from the app's manifest, so any PR that changes an app also bumps its version.
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
- **Logging:** logs hold only session metadata (member, line, start, end, bytes). Destinations and content are never logged.

## Language

Write code, comments, commits, PRs and docs in English.
