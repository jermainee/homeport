// Renders the sign-in state machine into the window. Each state is one
// screen; actions are buttons with data-action, handled in main.ts.

import type { ErrorCode, Member, State } from "./signin";

const errorText: Record<ErrorCode, string> = {
  invalid_email: "Enter a full email address, like name@example.com.",
  offline: "Homeport can't be reached. Check your internet connection and try again.",
  rate_limited: "Too many sign-in attempts. Wait a minute, then try again.",
  email_unavailable: "Sign-in emails can't be sent right now. Try again in a few minutes.",
  no_sign_in: "That sign-in was cancelled. Enter your email to start again.",
  unexpected: "Sign-in didn't work. Try again.",
};

const esc = (s: string) =>
  s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);

/** Groups a match code as "482 913", the way a house number is read. */
export function groupDigits(code: string): string {
  return code.length === 6 ? `${code.slice(0, 3)} ${code.slice(3)}` : code;
}

function emailForm(email: string, error: ErrorCode | undefined, sending: boolean): string {
  const invalid = error !== undefined;
  return `
    <form class="email-form" data-form="email" novalidate>
      <label for="email">Email</label>
      <input id="email" name="email" type="email" inputmode="email"
        autocomplete="email" autocapitalize="off" spellcheck="false"
        placeholder="name@example.com" value="${esc(email)}" required
        ${invalid ? 'aria-invalid="true" aria-describedby="email-error"' : ""}
        ${sending ? "readonly" : ""} />
      ${invalid ? `<p id="email-error" class="field-error" role="alert">${errorText[error]}</p>` : ""}
      <button class="button primary" type="submit" ${sending ? 'disabled aria-busy="true"' : ""}>
        ${sending ? "Sending link…" : "Email me a link"}
      </button>
    </form>`;
}

function signedOut(email: string, error: ErrorCode | undefined, sending: boolean): string {
  return `
    <section class="screen signin" aria-labelledby="title">
      <h1 id="title">Sign in</h1>
      <p class="lede">We'll email you a link to sign in. There's no password to remember.</p>
      ${emailForm(email, error, sending)}
      <p class="footnote">New to Homeport? The same link creates your account.</p>
    </section>`;
}

function waiting(s: Extract<State, { kind: "waiting" }>): string {
  const until = new Date(s.expiresAt).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  return `
    <section class="screen waiting" aria-labelledby="title">
      <h1 id="title">Check your inbox</h1>
      <p class="lede">We sent a sign-in link to <strong>${esc(s.email)}</strong>. Open it and check that the page shows this number:</p>
      <figure class="plate" role="img" aria-label="Match code ${s.matchCode.split("").join(" ")}">
        <span class="plate-digits" aria-hidden="true">${groupDigits(s.matchCode)}</span>
      </figure>
      <p class="status ${s.offline ? "is-offline" : ""}">
        <span class="beacon" aria-hidden="true"></span>
        ${s.offline ? "Homeport can't be reached. Trying again…" : "Waiting for you to confirm…"}
      </p>
      <div class="actions">
        <button class="button quiet" type="button" data-action="resend">Send a new link</button>
        <button class="button quiet" type="button" data-action="change-email">Use a different email</button>
      </div>
      <p class="footnote">The link works until ${esc(until)}.</p>
    </section>`;
}

function expired(email: string): string {
  return `
    <section class="screen expired" aria-labelledby="title">
      <h1 id="title">This link has expired</h1>
      <p class="lede">Sign-in links work for 15 minutes. Send a new one to <strong>${esc(email)}</strong>.</p>
      <div class="actions stacked">
        <button class="button primary" type="button" data-action="resend">Send a new link</button>
        <button class="button quiet" type="button" data-action="change-email">Use a different email</button>
      </div>
    </section>`;
}

function unreachable(error: ErrorCode): string {
  const offline = error === "offline";
  return `
    <section class="screen unreachable" aria-labelledby="title">
      <h1 id="title">${offline ? "Homeport can't be reached" : "Homeport didn't open"}</h1>
      <p class="lede">${offline ? "Check your internet connection, then try again." : "Something went wrong while loading your account. Try again."}</p>
      <div class="actions stacked">
        <button class="button primary" type="button" data-action="retry">Try again</button>
      </div>
    </section>`;
}

const credits = new Intl.NumberFormat();

function identityText(status: string): string {
  switch (status) {
    case "verified":
      return "Verified";
    case "processing":
      return "Being checked";
    case "requires_input":
      return "Needs another try";
    default:
      return "Not verified";
  }
}

function home(member: Member, signingOut: boolean): string {
  const c = member.credits;
  const total = c.purchased + c.earnedPending + c.earnedCashable;
  return `
    <section class="screen home" aria-labelledby="title">
      <h1 id="title" class="visually-hidden">Home</h1>

      <ul class="group" aria-label="Modes">
        <li class="row">
          <div class="row-text">
            <h2>Share</h2>
            <p>Let other members connect through this Mac's home line. You earn 5 credits per MB.</p>
          </div>
          <span class="soon">Coming soon</span>
        </li>
        <li class="row">
          <div class="row-text">
            <h2>Use</h2>
            <p>Connect through a member's home line with the WireGuard app. It costs 10 credits per MB.</p>
          </div>
          <span class="soon">Coming soon</span>
        </li>
      </ul>

      <h2 class="group-title">Account</h2>
      <dl class="group facts">
        <div class="row"><dt>Email</dt><dd>${esc(member.email)}</dd></div>
        <div class="row"><dt>Identity</dt><dd>${identityText(member.identityStatus)}</dd></div>
        <div class="row"><dt>Credits</dt><dd class="numeric">${credits.format(total)}</dd></div>
      </dl>

      <div class="actions">
        <button class="button quiet" type="button" data-action="sign-out" ${signingOut ? 'disabled aria-busy="true"' : ""}>
          ${signingOut ? "Signing out…" : "Sign out"}
        </button>
      </div>
    </section>`;
}

/** render returns the screen for a state. */
export function render(s: State): string {
  switch (s.kind) {
    case "checking":
      return `<section class="screen checking" aria-busy="true"></section>`;
    case "unreachable":
      return unreachable(s.error);
    case "signedOut":
      return signedOut(s.email, s.error, false);
    case "sending":
      return signedOut(s.email, undefined, true);
    case "waiting":
      return waiting(s);
    case "expired":
      return expired(s.email);
    case "signedIn":
      return home(s.member, false);
    case "signingOut":
      return home(s.member, true);
  }
}
