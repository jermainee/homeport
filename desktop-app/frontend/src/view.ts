// Renders the sign-in, Share and Use state machines into the window. Each
// sign-in state is one screen; home holds a tab for Share and one for Use.
// Actions are buttons with data-action, handled in ui.ts. Every value from
// Go or the API goes through esc().

import type { IdentityView } from "./identity";
import type { ShareStatus, ShareView } from "./share";
import type { UseView } from "./use";
import { identityParts, usePanel } from "./view-use";
import type { ErrorCode, Member, State } from "./signin";
import { errorText, esc, formatBytes, whole } from "./text";

export { formatBytes } from "./text";

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

// The resend button stays focusable while a link is on its way
// (aria-disabled, not disabled), so keyboard focus doesn't fall to the page.
function resendButton(style: "primary" | "quiet", resending: boolean | undefined): string {
  return `<button class="button ${style}" type="button" data-action="resend"
    ${resending ? 'aria-disabled="true" aria-busy="true"' : ""}>
    ${resending ? "Sending a new link…" : "Send a new link"}
  </button>`;
}

function resendError(error: ErrorCode | undefined): string {
  return error ? `<p class="field-error resend-error" role="alert">The new link wasn't sent. ${errorText[error]}</p>` : "";
}

function waiting(s: Extract<State, { kind: "waiting" }>): string {
  const until = new Date(s.expiresAt).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  return `
    <section class="screen waiting" aria-labelledby="title">
      <h1 id="title">Check your inbox</h1>
      <p class="lede">We sent a sign-in link to <strong>${esc(s.email)}</strong>. Open it and check that the page shows this number:</p>
      <figure class="plate" role="img" aria-label="Match code ${esc(s.matchCode.split("").join(" "))}">
        <span class="plate-digits" aria-hidden="true">${esc(groupDigits(s.matchCode))}</span>
      </figure>
      <p class="status ${s.offline ? "is-offline" : ""}">
        <span class="beacon" aria-hidden="true"></span>
        ${s.offline ? "Homeport can't be reached. Trying again…" : "Waiting for you to confirm…"}
      </p>
      <div class="actions">
        ${resendButton("quiet", s.resending)}
        <button class="button quiet" type="button" data-action="change-email">Use a different email</button>
      </div>
      ${resendError(s.error)}
      <p class="footnote">The link works until ${esc(until)}.</p>
    </section>`;
}

function expired(s: Extract<State, { kind: "expired" }>): string {
  return `
    <section class="screen expired" aria-labelledby="title">
      <h1 id="title">This link has expired</h1>
      <p class="lede">Sign-in links work for 15 minutes. Send a new one to <strong>${esc(s.email)}</strong>.</p>
      <div class="actions stacked">
        ${resendButton("primary", s.resending)}
        <button class="button quiet" type="button" data-action="change-email">Use a different email</button>
      </div>
      ${resendError(s.error)}
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

// The window glyph at the head of the Share panel: a house window whose
// light is on while the line is shared.
type Light = "off" | "on" | "warming" | "dim" | "fault";

function windowGlyph(light: Light): string {
  return `<span class="house-window" data-light="${light}" aria-hidden="true"><i></i><i></i><i></i><i></i></span>`;
}

function lightFor(st: ShareStatus): Light {
  switch (st.state) {
    case "online":
      return "on";
    case "connecting":
      return "warming";
    case "paused":
      return "dim";
    case "error":
      return "fault";
    default:
      return "off";
  }
}

/** shareLine says what Share is doing, in one sentence under the heading. */
export function shareLine(st: ShareStatus): string {
  switch (st.state) {
    case "connecting":
      return "Connecting…";
    case "online":
      return "Online. Members can use this Mac's line.";
    case "paused":
      return "Paused while another VPN is on. Share carries on once it's off.";
    case "offline":
      if (st.error === "gateway_unavailable") return "Waiting for a Homeport gateway. Trying again…";
      if (st.error === "offline") return "Can't reach Homeport. Trying again…";
      if (st.error === "rate_limited") return "Homeport asked Share to wait. Trying again later…";
      return "Homeport isn't answering. Trying again…";
    case "error":
      return "Share couldn't set up this Mac's line, so it stopped. Turn it on to try again.";
    default:
      return "Let verified members use this Mac's home line, and earn credits for what they use.";
  }
}

function shareSwitch(on: boolean, turning: boolean): string {
  return `<button class="switch" type="button" role="switch" data-action="share-toggle"
    aria-checked="${on}" aria-labelledby="share-title" aria-describedby="share-line"
    ${turning ? 'aria-disabled="true" aria-busy="true"' : ""}><span class="knob"></span></button>`;
}

function verifyText(identity: string): { line: string; note: string } {
  switch (identity) {
    case "processing":
      return {
        line: "Your identity is being checked",
        note: "This usually takes a few minutes. Share can turn on once it's done.",
      };
    case "requires_input":
      return {
        line: "Your identity check didn't go through",
        note: "Homeport checks every member's ID and a selfie once, before Share or Use turns on. Try the check again.",
      };
    default:
      return {
        line: "Verify your identity first",
        note: "Homeport checks every member's ID and a selfie once, before Share or Use turns on. That way every session traces back to a verified person.",
      };
  }
}

/**
 * sharePanel renders the Share state machine on the home screen. While Share
 * asks the member to verify, identity says where the check stands.
 */
export function sharePanel(v: ShareView, identity?: IdentityView): string {
  switch (v.kind) {
    case "loading":
    case "signedOut":
      return `
      <section class="share-panel" aria-labelledby="share-title" aria-busy="true">
        <div class="share-head">${windowGlyph("off")}<div class="share-text"><h2 id="share-title">Share</h2></div></div>
      </section>`;
    case "verify": {
      if (identity?.kind === "status" && identity.state !== "verified") {
        const p = identityParts(identity);
        return `
      <section class="share-panel is-verify" aria-labelledby="share-title">
        <div class="share-head">
          ${windowGlyph("off")}
          <div class="share-text">
            <h2 id="share-title">Share</h2>
            <p class="share-line" id="share-line">${p.line}</p>
          </div>
        </div>
        ${p.note ? `<p class="share-note">${p.note}</p>` : ""}
        <div class="share-identity">${p.actions}</div>
      </section>`;
      }
      // The check says verified, but Share's last answer from Go didn't.
      const t = verifyText(v.identity);
      return `
      <section class="share-panel is-verify" aria-labelledby="share-title">
        <div class="share-head">
          ${windowGlyph("off")}
          <div class="share-text">
            <h2 id="share-title">Share</h2>
            <p class="share-line" id="share-line">${t.line}</p>
          </div>
        </div>
        <p class="share-note">${t.note}</p>
        <div class="share-actions">
          <button class="button quiet" type="button" data-action="share-recheck"
            ${v.checking ? 'aria-disabled="true" aria-busy="true"' : ""}>
            ${v.checking ? "Checking…" : "Check again"}
          </button>
        </div>
      </section>`;
    }
    case "ready": {
      const st = v.status;
      const on = v.turning ? !st.sharing : st.sharing;
      const line = v.turning ? (on ? "Turning on…" : "Turning off…") : shareLine(st);
      const problem = !v.turning && (st.state === "offline" || st.state === "error");
      const flows = st.state === "online"
        ? `<div class="row"><dt>Connections now</dt><dd class="numeric" data-field="flowsActive">${whole.format(st.flowsActive)}</dd></div>`
        : "";
      return `
      <section class="share-panel" data-state="${st.state}" aria-labelledby="share-title">
        <div class="share-head">
          ${windowGlyph(v.turning ? "off" : lightFor(st))}
          <div class="share-text">
            <h2 id="share-title">Share</h2>
            <p class="share-line ${problem ? "is-problem" : ""}" id="share-line">${line}</p>
          </div>
          ${shareSwitch(on, v.turning)}
        </div>
        ${v.failed ? `<p class="field-error share-failed" role="alert">Share didn't turn ${st.sharing ? "off" : "on"}. ${v.failed === "unexpected" ? "Try again." : errorText[v.failed]}</p>` : ""}
        <dl class="share-facts">
          <div class="row"><dt>Shared today</dt><dd class="numeric" data-field="bytesToday">${formatBytes(st.bytesToday)}</dd></div>
          ${flows}
        </dl>
      </section>
      ${st.sharing || st.bytesToday > 0 ? `<p class="share-footnote">Counted on this Mac. Your credits follow Homeport's own count.</p>` : ""}`;
    }
  }
}

export type Tab = "share" | "use";

/** What home shows: the open tab, and the Share, identity and Use views. */
export interface HomeView {
  tab: Tab;
  share: ShareView;
  identity?: IdentityView;
  use: UseView;
}

const loadingHome: HomeView = { tab: "share", share: { kind: "loading" }, use: { kind: "loading" } };

function tab(t: Tab, label: string, open: Tab): string {
  const selected = t === open;
  return `<button class="tab" type="button" role="tab" id="tab-${t}" aria-controls="home-panel"
    aria-selected="${selected}" tabindex="${selected ? 0 : -1}" data-action="tab" data-tab="${t}">${label}</button>`;
}

function home(member: Member, signingOut: boolean, h: HomeView): string {
  const c = member.credits;
  // Use's balance is the newest; the member's is from when home opened.
  const total = h.use.kind === "ready" ? h.use.balance.total : c.purchased + c.earnedPending + c.earnedCashable;
  const panel =
    h.tab === "use"
      ? `<div class="use" data-use>${usePanel(h.use, h.identity)}</div>`
      : `<div class="share" data-share>${sharePanel(h.share, h.identity)}</div>`;
  return `
    <section class="screen home" aria-labelledby="title">
      <h1 id="title" class="visually-hidden">Home</h1>

      <div class="tabs" role="tablist" aria-label="Mode">
        ${tab("share", "Share", h.tab)}
        ${tab("use", "Use", h.tab)}
      </div>

      <div class="tab-panel" id="home-panel" role="tabpanel" aria-labelledby="tab-${h.tab}" data-panel="${h.tab}">
        ${panel}
      </div>

      <h2 class="group-title">Account</h2>
      <dl class="group facts">
        <div class="row"><dt>Email</dt><dd>${esc(member.email)}</dd></div>
        <div class="row"><dt>Identity</dt><dd>${identityText(identityOf(member, h.identity))}</dd></div>
        <div class="row"><dt>Credits</dt><dd class="numeric">${credits.format(total)}</dd></div>
      </dl>

      <div class="actions">
        <button class="button quiet" type="button" data-action="sign-out" ${signingOut ? 'disabled aria-busy="true"' : ""}>
          ${signingOut ? "Signing out…" : "Sign out"}
        </button>
      </div>
    </section>`;
}

// identityOf is the member's identity status: the check's, once it ran.
function identityOf(member: Member, identity: IdentityView | undefined): string {
  if (identity?.kind !== "status") return member.identityStatus;
  return identity.state === "waiting" ? member.identityStatus : identity.state;
}

/** render returns the screen for a state; home shows share as well. */
export function render(s: State, h: HomeView = loadingHome): string {
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
      return expired(s);
    case "signedIn":
      return home(s.member, false, h);
    case "signingOut":
      return home(s.member, true, h);
  }
}
