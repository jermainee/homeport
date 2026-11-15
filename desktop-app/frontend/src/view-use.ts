// Renders the identity check and the Use screen. Every value from Go or the
// API goes through esc(); actions are buttons with data-action, handled in
// ui.ts.

import type { IdentityView } from "./identity";
import type { ErrorCode } from "./signin";
import { countryName, esc, formatAbout, useErrorText, whole } from "./text";
import { backupLine, FAILOVERS, type Checkout, type Device, type ExitLine, type Failover, type Line, type Ready, type UseView } from "./use";

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
  // While a check is in progress, a beacon shows the app is following it;
  // once polling gave up, or while Check again asks, the button takes its
  // place.
  const following = (text: string) => (v.stalled || checking ? checkButton : beacon(text));

  let parts: IdentityParts;
  switch (v.state) {
    case "waiting":
      parts = {
        line: "Finish the check in your browser",
        note: "Stripe asks for a photo of your ID and a selfie. This screen moves on by itself once you're done.",
        // A member who closed Stripe's page can start over at once.
        actions: `<div class="panel-actions">${following("Waiting for the check…")}
          <button class="button quiet" type="button" data-action="identity-verify" ${busyAttrs(starting)}>${
            starting ? "Opening the check…" : "Start a new check"
          }</button></div>`,
      };
      break;
    case "processing":
      parts = {
        line: "Your identity is being checked",
        note: "This usually takes a few minutes. Share and Use turn on once it's done.",
        actions: `<div class="panel-actions">${following("Checking…")}</div>`,
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

/** Where a backup line is, as far as Homeport knows: "Potsdam, Germany", or "". */
function exitPlace(e: ExitLine): string {
  return [e.city, e.country ? countryName(e.country) : ""].filter(Boolean).join(", ");
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
        case "edit":
          return editStep(v, v.step);
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
  // Buy credits stays available while the app waits, so a member who left
  // Checkout can open a new one.
  const button = `<button class="button primary" type="button" data-action="use-buy" ${busyAttrs(opening)}>
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
      <div class="row-actions">
        <button class="button quiet" type="button" data-action="use-edit" data-peer="${esc(d.peerId)}"
          aria-label="Change ${esc(d.name)}">Change</button>
        <button class="button quiet" type="button" data-action="use-remove" data-peer="${esc(d.peerId)}"
          aria-label="Remove ${esc(d.name)}">Remove</button>
      </div>
      ${backupNotice(d)}
    </li>`;
}

// While its own line is offline, the gateway runs a device on another
// member's line: a window lit in another house.
function backupNotice(d: Device): string {
  const backup = backupLine(d);
  if (!backup) return "";
  const place = exitPlace(backup);
  const text = place
    ? `Its line went offline, so it's using a backup line in ${place}.`
    : "Its line went offline, so it's using a backup line.";
  return `<p class="device-backup"><span class="house-window is-tiny" data-light="on" aria-hidden="true"><i></i><i></i><i></i><i></i></span>${esc(text)}</p>`;
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

// The lines to pick from, for a new device or one that changes its line.
function linePicker(lines: Line[], picked: string | null): string {
  return `<fieldset class="lines">
        <legend class="label">Line</legend>
        <div class="group line-list">${lines.map((l) => lineOption(l, l.lineId === picked)).join("")}</div>
      </fieldset>`;
}

function addStep(v: Ready, s: Extract<Ready["step"], { kind: "add" }>): string {
  if (v.lines.length === 0) {
    return `
    <section class="panel use-step" aria-labelledby="add-title">
      <div class="step-head">
        <h2 id="add-title" tabindex="-1">Add a device</h2>
        <p class="panel-line empty">Nobody shares a line you can use right now. Lines appear here when members turn Share on.</p>
      </div>
      <div class="panel-actions">
        <button class="button quiet" type="button" data-action="use-cancel-add">Back</button>
      </div>
    </section>`;
  }
  const nameError = s.error === "invalid_name";
  const lines = linePicker(v.lines, s.lineId);
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

/**
 * What each failover does, named after the picked line's city and country
 * when Homeport knows them: "Use another line in Lyon only".
 */
export function failoverLabel(f: Failover, line: Pick<Line, "city" | "country"> | undefined): string {
  const city = line?.city ?? "";
  const country = line?.country ? countryName(line.country) : "";
  switch (f) {
    case "country":
      if (city && country) return `Use another line in ${city}, or else in ${country}`;
      if (country) return `Use another line in ${country}`;
      return "Use another line in the same city, or else the same country";
    case "city":
      return city ? `Use another line in ${city} only` : "Use another line in the same city only";
    case "off":
      return "Wait until its line is back";
  }
}

function failoverOption(f: Failover, picked: boolean, line: Line | undefined): string {
  return `
    <label class="failover-option">
      <input type="radio" name="failover" value="${f}" ${picked ? "checked" : ""} />
      <span class="failover-text">${esc(failoverLabel(f, line))}${
        f === "off" ? `<span class="failover-meta">It has no connection meanwhile.</span>` : ""
      }</span>
    </label>`;
}

function editStep(v: Ready, s: Extract<Ready["step"], { kind: "edit" }>): string {
  const d = v.devices.find((d) => d.peerId === s.peerId);
  const name = d?.name ?? "device";
  const picked = v.lines.find((l) => l.lineId === s.lineId);
  const lines = v.lines.length
    ? linePicker(v.lines, s.lineId)
    : `<p class="panel-line empty">Nobody shares another line you can use right now.</p>`;
  return `
    <section class="panel use-step" aria-labelledby="edit-title">
      <div class="step-head">
        <h2 id="edit-title" tabindex="-1">Change ${esc(name)}</h2>
        <p class="panel-line">It keeps its config, so there's nothing to scan or import again.</p>
      </div>
      <form class="add-form" data-form="edit-device" novalidate>
        ${lines}
        ${alert(s.error)}
        <fieldset class="failover">
          <legend class="label">If its line goes offline</legend>
          <div class="group failover-list">${FAILOVERS.map((f) => failoverOption(f, f === s.failover, picked)).join("")}</div>
          <p class="panel-hint">It moves back once its own line has been online again for a few minutes.</p>
        </fieldset>
        <div class="panel-actions">
          <button class="button primary" type="submit" data-focus="use-save-device" data-peer="${esc(s.peerId)}" ${busyAttrs(s.saving)}>
            ${s.saving ? "Saving…" : "Save changes"}
          </button>
          <button class="button quiet" type="button" data-action="use-cancel-edit" data-peer="${esc(s.peerId)}" ${s.saving ? "disabled" : ""}>Cancel</button>
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
