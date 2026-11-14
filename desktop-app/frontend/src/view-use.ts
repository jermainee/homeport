// Renders the identity check and the Use screen. Every value from Go or the
// API goes through esc(); actions are buttons with data-action, handled in
// ui.ts.

import type { IdentityView } from "./identity";
import type { ErrorCode } from "./signin";
import { countryName, esc, formatAbout, useErrorText, whole } from "./text";
import type { Checkout, Device, Line, Ready, UseView } from "./use";

const busyAttrs = (busy: boolean) => (busy ? 'aria-disabled="true" aria-busy="true"' : "");

function alert(error: ErrorCode | undefined, text = useErrorText): string {
  return error ? `<p class="field-error panel-error" role="alert">${esc(text(error))}</p>` : "";
}

/** The words for one state of the identity check, under a panel's title. */
export interface IdentityParts {
  line: string;
  note: string;
  actions: string;
}

function identityError(code: ErrorCode): string {
  // The API allows a few checks a day; Stripe bills each one.
  return code === "rate_limited"
    ? "You've started as many checks as Homeport allows in a day. Try again tomorrow."
    : useErrorText(code);
}

/** identityParts says where the identity check stands and offers the next step. */
export function identityParts(v: Extract<IdentityView, { kind: "status" }>): IdentityParts {
  const starting = v.busy === "starting";
  const checking = v.busy === "checking";
  const verifyButton = (label: string) => `
    <button class="button primary" type="button" data-action="identity-verify" ${busyAttrs(starting)}>
      ${starting ? "Opening the check…" : label}
    </button>`;
  const checkButton = `
    <button class="button quiet" type="button" data-action="identity-check" ${busyAttrs(checking)}>
      ${checking ? "Checking…" : "Check again"}
    </button>`;
  const beacon = (text: string) =>
    `<p class="status ${v.offline ? "is-offline" : ""}"><span class="beacon" aria-hidden="true"></span>${
      v.offline ? "Homeport can't be reached. Trying again…" : text
    }</p>`;
  const following = (text: string) =>
    v.stalled || v.busy ? `<div class="panel-actions">${checkButton}</div>` : `<div class="panel-actions">${beacon(text)}</div>`;

  let parts: IdentityParts;
  switch (v.state) {
    case "waiting":
      parts = {
        line: "Finish the check in your browser",
        note: "Stripe asks for a photo of your ID and a selfie. This screen moves on by itself once you're done.",
        actions: following("Waiting for the check…"),
      };
      if (v.stalled) {
        parts.actions = `<div class="panel-actions">${checkButton}
          <button class="button quiet" type="button" data-action="identity-verify" ${busyAttrs(starting)}>${
            starting ? "Opening the check…" : "Start a new check"
          }</button></div>`;
      }
      break;
    case "processing":
      parts = {
        line: "Your identity is being checked",
        note: "This usually takes a few minutes. Share and Use turn on once it's done.",
        actions: following("Checking…"),
      };
      break;
    case "requires_input":
      parts = {
        line: "Your identity check didn't go through",
        note: "Stripe couldn't confirm your ID and selfie, or the check was left unfinished. Try again with your whole ID in view and good light.",
        actions: `<div class="panel-actions">${verifyButton("Try again")}</div>`,
      };
      break;
    case "verified":
      parts = { line: "Your identity is verified", note: "", actions: "" };
      break;
    default:
      parts = {
        line: "Verify your identity first",
        note: "Homeport checks every member's ID and a selfie once, before Share or Use turns on. That way every session traces back to a verified person.",
        actions: `<div class="panel-actions">${verifyButton("Verify identity")}<p class="panel-hint">The check opens in your browser.</p></div>`,
      };
  }
  return { ...parts, actions: parts.actions + alert(v.error, identityError) };
}

// The door at the head of the Use panel: another member's front door.
const doorGlyph = `<span class="door" aria-hidden="true"><i></i></span>`;

function head(line: string, problem = false): string {
  return `
    <div class="panel-head">
      ${doorGlyph}
      <div class="panel-text">
        <h2 id="use-title">Use</h2>
        <p class="panel-line ${problem ? "is-problem" : ""}" id="use-line">${line}</p>
      </div>
    </div>`;
}

/** Where a line is, as members know it: "Berlin, Germany". */
export function linePlace(l: Pick<Line, "city" | "country">): string {
  const country = countryName(l.country);
  return l.city ? `${l.city}, ${country}` : country;
}

/** usePanel renders the Use state machine; identity fills in while it asks to verify. */
export function usePanel(v: UseView, identity: IdentityView | undefined): string {
  switch (v.kind) {
    case "loading":
    case "signedOut":
      return `<section class="panel use-panel" aria-labelledby="use-title" aria-busy="true">${head("")}</section>`;
    case "verify": {
      if (identity?.kind !== "status" || identity.state === "verified") {
        // The API refused the member although the check says verified.
        return `
        <section class="panel use-panel is-verify" aria-labelledby="use-title">
          ${head("Use couldn't confirm your identity", true)}
          <div class="panel-actions"><button class="button quiet" type="button" data-action="use-unlock">Check again</button></div>
        </section>`;
      }
      const p = identityParts(identity);
      return `
        <section class="panel use-panel is-verify" aria-labelledby="use-title">
          ${head(p.line)}
          ${p.note ? `<p class="panel-note">${p.note}</p>` : ""}
          ${p.actions}
        </section>`;
    }
    case "failed":
      return `
        <section class="panel use-panel" aria-labelledby="use-title">
          ${head("Use couldn't load", true)}
          <p class="panel-note">${esc(useErrorText(v.error))}</p>
          <div class="panel-actions"><button class="button primary" type="button" data-action="use-retry">Try again</button></div>
        </section>`;
    case "ready":
      switch (v.step.kind) {
        case "add":
          return addStep(v, v.step);
        case "config":
          return configStep(v, v.step);
        default:
          return overview(v);
      }
  }
}

function balanceLine(v: Ready): string {
  const b = v.balance;
  if (b.total <= 0) return "No credits yet. Buy some to connect through a member's home line.";
  return `${whole.format(b.total)} credits, enough for about ${formatAbout(b.bytes)} of use.`;
}

function checkoutPart(c: Checkout): string {
  const opening = c.phase === "opening";
  const paying = c.phase === "waiting" || c.phase === "stalled";
  const button = `<button class="button primary" type="button" data-action="use-buy" ${busyAttrs(opening || paying)}>
      ${opening ? "Opening Checkout…" : "Buy credits"}
    </button>`;
  switch (c.phase) {
    case "waiting":
      return `<div class="panel-actions">${button}
        <p class="status ${c.offline ? "is-offline" : ""}"><span class="beacon" aria-hidden="true"></span>${
          c.offline ? "Homeport can't be reached. Trying again…" : "Finish paying in your browser."
        }</p></div>`;
    case "stalled":
      return `<div class="panel-actions">${button}
        <button class="button quiet" type="button" data-action="use-check-payment">Check again</button></div>
        <p class="panel-hint">No credits yet. Card payments arrive within a minute; bank debits can take a few days.</p>`;
    case "idle":
      return `<div class="panel-actions">${button}${
        c.arrived ? `<p class="status is-done"><span class="tick" aria-hidden="true"></span>Your credits arrived.</p>` : ""
      }</div>${alert(c.error)}`;
    default:
      return `<div class="panel-actions">${button}</div>`;
  }
}

function deviceRow(d: Device, v: Ready): string {
  const line = v.lines.find((l) => l.lineId === d.lineId);
  const where = line ? linePlace(line) : "Line not shared right now";
  const r = v.removing;
  if (r?.peerId === d.peerId) {
    return `
      <li class="row device is-confirming" data-peer="${esc(d.peerId)}">
        <div class="row-text">
          <h3>Remove ${esc(d.name)}?</h3>
          <p>It stops connecting within a minute. To use it again, add it again.</p>
          ${alert(r.error)}
        </div>
        <div class="row-actions">
          <button class="button danger" type="button" data-action="use-confirm-remove" data-peer="${esc(d.peerId)}" ${busyAttrs(r.busy)}>
            ${r.busy ? "Removing…" : "Remove"}
          </button>
          <button class="button quiet" type="button" data-action="use-cancel-remove" data-peer="${esc(d.peerId)}">Keep</button>
        </div>
      </li>`;
  }
  return `
    <li class="row device" data-peer="${esc(d.peerId)}">
      <div class="row-text">
        <h3>${esc(d.name)}</h3>
        <p>${esc(where)}</p>
      </div>
      <button class="button quiet" type="button" data-action="use-remove" data-peer="${esc(d.peerId)}"
        aria-label="Remove ${esc(d.name)}">Remove</button>
    </li>`;
}

function overview(v: Ready): string {
  const b = v.balance;
  const devices = v.devices.length
    ? `<ul class="group devices" aria-labelledby="devices-title">${v.devices.map((d) => deviceRow(d, v)).join("")}</ul>`
    : `<p class="empty">No devices yet. Add one to get a WireGuard config for an iPhone, a Mac or a travel router.</p>`;
  // With no credits, buying them is the next step; then adding a device.
  const firstDevice = v.devices.length === 0 && b.total > 0;
  return `
    <section class="panel use-panel" aria-labelledby="use-title">
      ${head(balanceLine(v))}
      ${
        b.total > 0
          ? `<dl class="panel-facts">
        <div class="row"><dt>Bought</dt><dd class="numeric" data-field="purchased">${whole.format(b.purchased)}</dd></div>
        <div class="row"><dt>Earned by sharing</dt><dd class="numeric" data-field="earned">${whole.format(b.earned)}</dd></div>
      </dl>`
          : ""
      }
      ${checkoutPart(v.checkout)}
    </section>
    <h2 class="group-title" id="devices-title">Devices</h2>
    ${devices}
    <div class="actions">
      <button class="button ${firstDevice ? "primary" : "quiet"}" type="button" data-action="use-add">Add a device</button>
    </div>`;
}

function lineOption(l: Line, picked: boolean): string {
  const meta = [l.isp, l.ipv6 ? "IPv6" : ""].filter(Boolean).join(", ");
  return `
    <label class="line-option ${l.online ? "" : "is-offline"}">
      <input type="radio" name="line" value="${esc(l.lineId)}" ${picked ? "checked" : ""} />
      <span class="house-window is-small" data-light="${l.online ? "on" : "off"}" aria-hidden="true"><i></i><i></i><i></i><i></i></span>
      <span class="line-text">
        <span class="line-place">${esc(linePlace(l))}</span>
        ${meta ? `<span class="line-meta">${esc(meta)}</span>` : ""}
      </span>
      <span class="line-state">${l.online ? "Online" : "Offline now"}</span>
    </label>`;
}

function addStep(v: Ready, s: Extract<Ready["step"], { kind: "add" }>): string {
  const nameError = s.error === "invalid_name";
  const lines = v.lines.length
    ? `<fieldset class="lines">
        <legend class="label">Line</legend>
        <div class="group line-list">${v.lines.map((l) => lineOption(l, l.lineId === s.lineId)).join("")}</div>
      </fieldset>`
    : `<p class="empty">Nobody shares a line you can use right now. Lines appear here when members turn Share on.</p>`;
  return `
    <section class="panel use-step" aria-labelledby="add-title">
      <div class="step-head">
        <h2 id="add-title" tabindex="-1">Add a device</h2>
        <p class="panel-line">Pick the home line it connects through. A lit window means the line is online now.</p>
      </div>
      <form class="add-form" data-form="add-device" novalidate>
        ${lines}
        ${s.error && !nameError ? alert(s.error) : ""}
        <label class="label" for="device-name">Device name</label>
        <input id="device-name" name="name" type="text" maxlength="64" autocomplete="off" spellcheck="false"
          placeholder="iPhone" value="${esc(s.name)}"
          ${nameError ? 'aria-invalid="true" aria-describedby="name-error"' : ""} ${s.creating ? "readonly" : ""} />
        ${nameError ? `<p id="name-error" class="field-error" role="alert">${esc(useErrorText("invalid_name"))}</p>` : ""}
        <div class="panel-actions">
          <button class="button primary" type="submit" data-focus="use-create" ${busyAttrs(s.creating)}>
            ${s.creating ? "Creating config…" : "Create config"}
          </button>
          <button class="button quiet" type="button" data-action="use-cancel-add" ${s.creating ? "disabled" : ""}>Cancel</button>
        </div>
      </form>
    </section>`;
}

function configStep(v: Ready, s: Extract<Ready["step"], { kind: "config" }>): string {
  const dev = s.device.device;
  const line = v.lines.find((l) => l.lineId === dev.lineId);
  return `
    <section class="panel use-step" aria-labelledby="config-title">
      <div class="step-head">
        <h2 id="config-title" tabindex="-1">Scan with WireGuard</h2>
        <p class="panel-line">On the iPhone, open the WireGuard app, tap +, then Create from QR code.</p>
      </div>
      <figure class="key-card" data-peer="${esc(dev.peerId)}">
        <img src="${esc(s.device.qrCode)}" width="232" height="232"
          alt="QR code of the WireGuard config for ${esc(dev.name)}" />
        <figcaption>
          <span class="key-name">${esc(dev.name)}</span>
          ${line ? `<span class="key-place">${esc(linePlace(line))}</span>` : ""}
        </figcaption>
      </figure>
      <p class="panel-note">On a Mac or a travel router, save the file and import it into WireGuard.</p>
      <div class="panel-actions">
        <button class="button quiet" type="button" data-action="use-save" ${busyAttrs(s.saving)}>
          ${s.saving ? "Saving…" : s.saved ? "Save another copy" : "Save .conf file"}
        </button>
        ${s.saved ? `<p class="status is-done"><span class="tick" aria-hidden="true"></span>Saved.</p>` : ""}
      </div>
      ${alert(s.error)}
      <p class="once"><strong>Homeport shows this config only once.</strong> It holds the device's private key, and Homeport keeps no copy. If you lose it, remove the device and add it again.</p>
      <div class="panel-actions">
        <button class="button primary" type="button" data-action="use-done">Done</button>
      </div>
    </section>`;
}
