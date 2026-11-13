import { html } from "hono/html";
import type { HtmlEscapedString } from "hono/utils/html";

type Html = HtmlEscapedString | Promise<HtmlEscapedString>;

/** Headers for every sign-in page: never cached, never framed, no referrer leaks the token. */
export const PAGE_HEADERS: Record<string, string> = {
  "cache-control": "no-store",
  "referrer-policy": "no-referrer",
  "x-content-type-options": "nosniff",
  "x-robots-tag": "noindex",
  "content-security-policy":
    "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
};

export function confirmPage(input: { email: string; matchCode: string; token: string }): Html {
  return layout(
    "Confirm sign-in",
    html`<h1>Sign in to Homeport</h1>
      <p>Signing in as <strong>${input.email}</strong>.</p>
      <p>Confirm only if the Homeport app on your computer shows this code:</p>
      <p class="code" aria-label="Match code">${formatMatchCode(input.matchCode)}</p>
      <form method="post" action="/auth/confirm">
        <input type="hidden" name="token" value="${input.token}" />
        <input type="hidden" name="match_code" value="${input.matchCode}" />
        <button type="submit">Confirm</button>
      </form>
      <p class="hint">If the codes differ, or you did not try to sign in, close this page.</p>`,
  );
}

export function messagePage(title: string, message: string): Html {
  return layout(title, html`<h1>${title}</h1><p>${message}</p>`);
}

function formatMatchCode(code: string): string {
  return `${code.slice(0, 3)} ${code.slice(3)}`;
}

function layout(title: string, body: Html): Html {
  return html`<!doctype html>
    <html lang="en">
      <head>
        <meta charset="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <title>${title} · Homeport</title>
        <style>
          :root { color-scheme: light dark; --ink: #1b1b1f; --paper: #fafaf7; --muted: #5d5d66; --accent: #1f5f4a; }
          @media (prefers-color-scheme: dark) { :root { --ink: #ececef; --paper: #141416; --muted: #a0a0aa; --accent: #6cc4a1; } }
          body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--paper); color: var(--ink);
                 font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; }
          main { max-width: 26rem; padding: 2rem 1rem; }
          h1 { font-size: 1.5rem; margin: 0 0 1rem; }
          .code { font: 600 2.25rem/1.2 ui-monospace, "SF Mono", Menlo, monospace; letter-spacing: 0.1em; margin: 1rem 0; }
          button { font: inherit; font-weight: 600; padding: 0.7rem 1.6rem; border: 0; border-radius: 0.5rem;
                   background: var(--accent); color: var(--paper); cursor: pointer; }
          button:focus-visible { outline: 3px solid var(--ink); outline-offset: 2px; }
          .hint { color: var(--muted); font-size: 0.9rem; margin-top: 1.5rem; }
        </style>
      </head>
      <body>
        <main>${body}</main>
      </body>
    </html>`;
}
