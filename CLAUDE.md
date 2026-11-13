@AGENTS.md

## Skills for this repo

- **Feature or bug fix:** run `/mattpocock-skills:tdd`.
- **PR with code or config changes, ready to merge:** run `/code-review <PR number>`. Fix every finding, or answer it in the PR, before merging.
- **Any UI** (landing page, app screens, dashboard): use `/frontend-design` to set the direction, then `/impeccable` to refine and audit it.
- **Cloudflare** (Pages, Workers, D1, Wrangler, deploy workflows): load `cloudflare:wrangler` before running Wrangler, and `cloudflare:workers-best-practices` when writing Worker code.
- **Stripe** (Identity, Checkout, Connect, Tax, webhooks): load `stripe:stripe-best-practices`. For Connect setup, also load `stripe:connect-recommend`.

## Product decisions

The pricing, credit, identity and trust rules come from the concept doc: https://claude.ai/artifact/3XH7c5NjRbqdE9W4cY8zgv. It is a Claude Docs document, so read it with the docs tools. When a change touches those rules, check it against the doc. If the doc has moved on, update the rules in AGENTS.md.
