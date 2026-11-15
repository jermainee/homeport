// Renders the identity check and the Use screen. Every value from Go or the
// API goes through esc(); actions are buttons with data-action, handled in
// ui.ts.

import type { IdentityView } from "./identity";
import type { ErrorCode } from "./signin";
import { countryName, esc, formatAbout, useErrorText, whole } from "./text";
import {
  backupLine,
  FAILOVERS,
  type AddStep,
  type Checkout,
  type City,
  type Device,
  type EditStep,
  type ExitLine,
  type Failover,
  type Ready,
  type Region,
  type RegionPick,
  type UseView,
} from "./use";

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
export function linePlace(l: Pick<Device, "city" | "country">): string {
  if (!l.country) return l.city || "Place not known";
  const country = countryName(l.country);
  return l.city ? `${l.city}, ${country}` : country;
}

/** Who provides a line, as a device row and the key card say it. */
function ispLine(isp: string): string {
  return isp ? `${isp} line` : "";
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
          return addStep(v.step);
        case "config":
          return configStep(v.step);
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
        <p>${esc(linePlace(d))}</p>
        ${d.isp ? `<p>${esc(ispLine(d.isp))}</p>` : ""}
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

/** How many cities the list shows before the member has to type. */
export const CITY_LIST = 8;

const byName = new Intl.Collator(undefined, { sensitivity: "base" });

/** Text compared without case or accents, so "kol" finds "Köln". */
const fold = (s: string) => s.normalize("NFD").replace(/\p{M}/gu, "").toLocaleLowerCase();

/**
 * The cities the list shows: the first CITY_LIST, most lines first, or with
 * a query those whose name holds it. The picked city stays in the list.
 */
export function visibleCities(cities: City[], query: string, picked: string): { shown: City[]; matches: number } {
  const q = fold(query.trim());
  const matching = q ? cities.filter((c) => fold(c.city).includes(q)) : cities;
  const shown = matching.slice(0, CITY_LIST);
  const pick = cities.find((c) => c.city === picked);
  if (pick && !shown.includes(pick)) shown.push(pick);
  return { shown, matches: matching.length };
}

const freeLines = (n: number) => `${whole.format(n)} free ${n === 1 ? "line" : "lines"}`;

// The lit panes of a city's window say roughly how many lines are free:
// one pane for one line, all four from four lines on. The id keeps a
// focused radio focused when the list is drawn again.
function cityOption(id: string, value: string, label: string, lines: number, picked: boolean): string {
  const lit = Math.min(4, Math.max(1, lines));
  return `
    <label class="line-option city-option">
      <input id="${id}" type="radio" name="city" value="${esc(value)}" ${picked ? "checked" : ""} />
      <span class="house-window is-small" data-lit="${lit}" aria-hidden="true"><i></i><i></i><i></i><i></i></span>
      <span class="line-text"><span class="line-place">${esc(label)}</span></span>
      <span class="line-state">${whole.format(lines)} free</span>
    </label>`;
}

// The countries by their names in the member's language, sorted once per
// list rather than on every redraw.
const sortedCountries = new WeakMap<Region[], { country: string; name: string; lines: number }[]>();

function byCountryName(countries: Region[]) {
  let sorted = sortedCountries.get(countries);
  if (!sorted) {
    sorted = countries.map((c) => ({ ...c, name: countryName(c.country) })).sort((a, b) => byName.compare(a.name, b.name));
    sortedCountries.set(countries, sorted);
  }
  return sorted;
}

/** The country select; `busy` while what was picked is being sent. */
function countryPicker(s: RegionPick, countries: Region[], busy: boolean, error: ErrorCode | undefined): string {
  return `
    <label class="label" for="device-country">Country</label>
    <select id="device-country" name="country" ${busy ? "disabled" : ""}
      ${error === "no_country" ? 'aria-invalid="true" aria-describedby="country-error"' : ""}>
      ${s.country === null ? '<option value="" selected disabled>Choose a country</option>' : ""}
      ${byCountryName(countries)
        .map((c) => `<option value="${esc(c.country)}" ${c.country === s.country ? "selected" : ""}>${esc(`${c.name}, ${freeLines(c.lines)}`)}</option>`)
        .join("")}
    </select>
    ${error === "no_country" ? `<p id="country-error" class="field-error" role="alert">${esc(useErrorText("no_country"))}</p>` : ""}`;
}

/** The cities of the picked country, with any city first. */
function cityPicker(s: RegionPick, country: Region, busy: boolean): string {
  const name = countryName(country.country);
  const any = cityOption("city-any", "", `Any city in ${name}`, country.lines, s.city === "");
  if (s.cities === null) {
    return `<fieldset class="lines cities" aria-busy="true">
        <legend class="label">City</legend>
        <div class="group line-list">${any}</div>
        <p class="status"><span class="beacon" aria-hidden="true"></span>Finding cities…</p>
      </fieldset>`;
  }
  const { shown, matches } = visibleCities(s.cities, s.query, s.city);
  const hidden = matches - CITY_LIST;
  const hint =
    hidden > 0
      ? `${whole.format(hidden)} more ${hidden === 1 ? "city" : "cities"}. Type to find one.`
      : s.query.trim() && matches === 0
        ? `No city in ${name} matches. Any city still works.`
        : "";
  const more = hint ? `<p id="city-more" class="panel-hint city-more">${esc(hint)}</p>` : "";
  const search =
    s.cities.length > CITY_LIST
      ? `<input id="city-query" type="search" autocomplete="off" spellcheck="false" aria-controls="city-list"
          aria-label="Find a city in ${esc(name)}" placeholder="Find a city in ${esc(name)}" value="${esc(s.query)}"
          ${hint ? 'aria-describedby="city-more"' : ""} />`
      : "";
  const failed = s.citiesError
    ? `${alert(s.citiesError)}<div class="panel-actions"><button class="button quiet" type="button" data-action="use-reload-cities">Load the cities again</button></div>`
    : "";
  // Each radio's id is its city's place in the full list, so it stays the
  // same while the member types.
  const all = s.cities;
  const options = shown.map((c) => cityOption(`city-${all.indexOf(c)}`, c.city, c.city, c.lines, c.city === s.city));
  // While what was picked is being sent, the pick stays on screen.
  return `<fieldset class="lines cities" ${busy ? "disabled" : ""}>
      <legend class="label">City</legend>
      ${search}
      <div class="group line-list" id="city-list">${any}${options.join("")}</div>
      ${more}
      ${failed}
    </fieldset>`;
}

/** The country and city pickers, once the countries are there. */
function regionPicker(s: RegionPick, countries: Region[], busy: boolean, error: ErrorCode | undefined): string {
  const country = countries.find((c) => c.country === s.country);
  return `${countryPicker(s, countries, busy, error)}${country ? cityPicker(s, country, busy) : ""}`;
}

const finding = `<p class="status"><span class="beacon" aria-hidden="true"></span>Finding where lines are free…</p>`;

function addStep(s: AddStep): string {
  const head = (line: string, cls = "") => `
      <div class="step-head">
        <h2 id="add-title" tabindex="-1">Add a device</h2>
        <p class="panel-line ${cls}">${line}</p>
      </div>`;
  const back = (label: string) =>
    `<button class="button quiet" type="button" data-action="use-cancel-add">${label}</button>`;
  if (s.countries === null) {
    return `
    <section class="panel use-step" aria-labelledby="add-title" aria-busy="true">
      ${head("Pick where its traffic should come out. Homeport finds a free home line there.")}
      <div class="panel-actions">
        ${finding}
        ${back("Cancel")}
      </div>
    </section>`;
  }
  if (s.countries.length === 0) {
    const failed = s.error !== undefined;
    return `
    <section class="panel use-step" aria-labelledby="add-title">
      ${failed ? head("Homeport couldn't find out where lines are free.") : head("Nobody shares a line you can use right now. Lines appear here when members turn Share on.", "empty")}
      ${failed ? alert(s.error) : ""}
      <div class="panel-actions">
        ${failed ? '<button class="button primary" type="button" data-action="use-reload-regions">Try again</button>' : ""}
        ${back(failed ? "Cancel" : "Back")}
      </div>
    </section>`;
  }
  const nameError = s.error === "invalid_name";
  const formError = s.error && s.error !== "invalid_name" && s.error !== "no_country" ? alert(s.error) : "";
  return `
    <section class="panel use-step" aria-labelledby="add-title">
      ${head("Pick where its traffic should come out. Homeport finds a free home line there.")}
      <form class="add-form" data-form="add-device" novalidate>
        ${regionPicker(s, s.countries, s.creating, s.error)}
        ${formError}
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
export function failoverLabel(f: Failover, line: { city: string; country: string } | undefined): string {
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

function failoverOption(f: Failover, picked: boolean, line: { city: string; country: string } | undefined): string {
  return `
    <label class="failover-option">
      <input id="failover-${f}" type="radio" name="failover" value="${f}" ${picked ? "checked" : ""} />
      <span class="failover-text">${esc(failoverLabel(f, line))}${
        f === "off" ? `<span class="failover-meta">It has no connection meanwhile.</span>` : ""
      }</span>
    </label>`;
}

// Where a moving device's line will be, once the member picked a region:
// the region's city and country, or nothing yet.
function movePlace(s: EditStep): { city: string; country: string } | undefined {
  return s.country ? { city: s.city, country: s.country } : undefined;
}

// Keep the device's line, or move it to another line in a region the
// member picks; the API picks the line there, on the device's gateway.
function moveChoice(s: EditStep, d: Device | undefined): string {
  const here = d ? [linePlace(d), ispLine(d.isp)].filter(Boolean).join(". ") : "";
  return `
    <fieldset class="failover move" ${s.saving ? "disabled" : ""}>
      <legend class="label">Line</legend>
      <div class="group failover-list">
        <label class="failover-option">
          <input id="move-keep" type="radio" name="move" value="keep" ${s.move ? "" : "checked"} />
          <span class="failover-text">Keep its line${here ? `<span class="failover-meta">${esc(here)}</span>` : ""}</span>
        </label>
        <label class="failover-option">
          <input id="move-other" type="radio" name="move" value="move" ${s.move ? "checked" : ""} />
          <span class="failover-text">Move it to another line<span class="failover-meta">Pick a country, and a city if you like. Homeport finds a free line there.</span></span>
        </label>
      </div>
    </fieldset>`;
}

// The region a moving device goes to, as the countries load or fail.
function moveRegion(s: EditStep): string {
  if (!s.move) return "";
  if (s.countries === null) return `<div class="panel-actions">${finding}</div>`;
  if (s.countries.length === 0) {
    return s.error && s.error !== "no_free_line"
      ? `<div class="panel-actions"><button class="button quiet" type="button" data-action="use-reload-regions">Try again</button></div>`
      : `<p class="panel-line empty">Nobody shares another line you can use right now.</p>`;
  }
  return regionPicker(s, s.countries, s.saving, s.error);
}

function editStep(v: Ready, s: EditStep): string {
  const d = v.devices.find((d) => d.peerId === s.peerId);
  const name = d?.name ?? "device";
  // The failover follows the line the device will be on.
  const place = s.move ? movePlace(s) : d;
  return `
    <section class="panel use-step" aria-labelledby="edit-title">
      <div class="step-head">
        <h2 id="edit-title" tabindex="-1">Change ${esc(name)}</h2>
        <p class="panel-line">It keeps its config, so there's nothing to scan or import again.</p>
      </div>
      <form class="add-form" data-form="edit-device" novalidate>
        ${moveChoice(s, d)}
        ${moveRegion(s)}
        ${s.error && s.error !== "no_country" ? alert(s.error) : ""}
        <fieldset class="failover">
          <legend class="label">If its line goes offline</legend>
          <div class="group failover-list">${FAILOVERS.map((f) => failoverOption(f, f === s.failover, place)).join("")}</div>
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

function configStep(s: Extract<Ready["step"], { kind: "config" }>): string {
  const dev = s.device.device;
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
          <span class="key-place">${esc(linePlace(dev))}</span>
          ${dev.isp ? `<span class="key-isp">${esc(ispLine(dev.isp))}</span>` : ""}
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
