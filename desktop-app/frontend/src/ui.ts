// Connects the sign-in, identity, Share and Use state machines to the
// window: renders each state, handles the buttons and forms, moves focus
// and announces changes.

import { Identity, type IdentityApi, type IdentityView } from "./identity";
import { Share, type ShareApi, type ShareView } from "./share";
import type { SignIn, State } from "./signin";
import { asFailover, DEVICES_REFRESH_MS, Use, type Step, type UseApi, type UseView } from "./use";
import { render, shareLine, type HomeView, type Tab } from "./view";
import { identityParts } from "./view-use";

/** The Go services home drives, through their seams. */
export interface HomeApis {
  share: ShareApi;
  identity: IdentityApi;
  use: UseApi;
}

// A new screen, or a new match code on the same one. Only then does the
// plate hang into place (see #app[data-fresh] in style.css).
function screenKey(s: State): string {
  return s.kind === "waiting" ? `waiting:${s.matchCode}` : s.kind;
}

// What a screen reader should hear when the screen stays but its news
// changes. A live region inside #app would be replaced with every render.
function announcement(prev: State | undefined, next: State): string | undefined {
  if (next.kind !== "waiting" || prev?.kind !== "waiting") return undefined;
  if (next.offline !== prev.offline) {
    return next.offline ? "Homeport can't be reached. Trying again." : "Connected again. Waiting for you to confirm.";
  }
  return undefined;
}

// What a screen reader should hear when Share's state changes on its own.
function shareAnnouncement(prev: ShareView | undefined, next: ShareView): string | undefined {
  if (next.kind !== "ready" || next.turning || prev?.kind !== "ready") return undefined;
  if (prev.status.state === next.status.state && !prev.turning) return undefined;
  return `Share: ${shareLine(next.status)}`;
}

function identityAnnouncement(prev: IdentityView | undefined, next: IdentityView): string | undefined {
  if (next.kind !== "status" || prev?.kind !== "status" || next.busy) return undefined;
  if (prev.state === next.state && prev.offline === next.offline) return undefined;
  if (next.offline) return "Homeport can't be reached. Trying again.";
  return identityParts(next).line + ".";
}

function useAnnouncement(prev: UseView | undefined, next: UseView): string | undefined {
  if (next.kind !== "ready" || prev?.kind !== "ready") return undefined;
  const was = prev.checkout;
  const now = next.checkout;
  if (now.phase === "idle" && now.arrived && !(was.phase === "idle" && was.arrived)) return "Your credits arrived.";
  if (now.phase === "stalled" && was.phase !== "stalled") return "No credits yet.";
  if (next.step.kind === "config" && next.step.saved && !(prev.step.kind === "config" && prev.step.saved)) return "Saved.";
  if (next.devices.length < prev.devices.length) return "Device removed.";
  if (prev.step.kind === "edit" && prev.step.saving && next.step.kind === "list") return "Changes saved.";
  return undefined;
}

// sameShape reports whether two elements have the same tags in the same
// places, so one can become the other by attributes and text alone.
function sameShape(a: Element, b: Element): boolean {
  if (a.tagName !== b.tagName || a.children.length !== b.children.length) return false;
  for (let i = 0; i < a.children.length; i++) {
    if (!sameShape(a.children[i]!, b.children[i]!)) return false;
  }
  return true;
}

// sync gives live the attributes and leaf text of next. Unchanged values are
// left alone, so CSS transitions run only for what changed.
function sync(live: Element, next: Element): void {
  for (const { name } of [...live.attributes]) {
    if (!next.hasAttribute(name)) live.removeAttribute(name);
  }
  for (const { name, value } of [...next.attributes]) {
    if (live.getAttribute(name) !== value) live.setAttribute(name, value);
  }
  if (next.children.length === 0) {
    if (live.textContent !== next.textContent) live.textContent = next.textContent;
    return;
  }
  for (let i = 0; i < next.children.length; i++) sync(live.children[i]!, next.children[i]!);
}

// morph updates the part of the window that selector finds in place when
// its markup keeps its shape. The Share switch then slides, the window
// light fades, and focus, typed text and a screen reader's place survive
// the updates Go sends every second while sharing.
function morph(app: HTMLElement, html: string, selector: string): boolean {
  const live = app.querySelector(selector);
  if (!live) return false;
  const tpl = document.createElement("template");
  tpl.innerHTML = html;
  const next = tpl.content.querySelector(selector);
  if (!next || !sameShape(live, next)) return false;
  sync(live, next);
  return true;
}

// The control that had focus, so it gets focus back after a redraw: its
// action (data-action, or data-focus on a form's submit button), and the
// device or tab it acts on; or, for a field, radio or heading without an
// action, its id and where the caret was.
interface FocusKey {
  attr: "data-action" | "data-focus" | "id";
  action: string;
  peer?: string;
  tab?: string;
  caret?: [number, number];
}

function focusKey(el: Element | null): FocusKey | undefined {
  const h = el as HTMLElement | null;
  const d = h?.dataset;
  if (d?.action) return { attr: "data-action", action: d.action, peer: d.peer, tab: d.tab };
  if (d?.focus) return { attr: "data-focus", action: d.focus, peer: d.peer, tab: d.tab };
  if (!h?.id) return undefined;
  const caret =
    h instanceof HTMLInputElement && h.selectionStart !== null && h.selectionEnd !== null
      ? ([h.selectionStart, h.selectionEnd] as [number, number])
      : undefined;
  return { attr: "id", action: h.id, caret };
}

function selectorFor(k: FocusKey): string {
  if (k.attr === "id") return `#${CSS.escape(k.action)}`;
  let sel = `[${k.attr}="${k.action}"]`;
  if (k.peer) sel += `[data-peer="${CSS.escape(k.peer)}"]`;
  if (k.tab) sel += `[data-tab="${k.tab}"]`;
  return sel;
}

// Where focus goes when the control the member used is gone: to what the
// action opened, or back to where the flow started.
function fallbackFor(k: FocusKey): string | undefined {
  const peer = k.peer ? `[data-peer="${CSS.escape(k.peer)}"]` : "";
  switch (k.action) {
    case "use-add":
      return "#add-title";
    case "use-create":
      return "#config-title";
    case "use-cancel-add":
    case "use-done":
    case "use-confirm-remove":
    // The device was removed elsewhere, so its switch is gone.
    case "use-https-only":
      return '[data-action="use-add"]';
    case "use-remove":
      return `[data-action="use-cancel-remove"]${peer}`;
    case "use-cancel-remove":
      return `[data-action="use-remove"]${peer}`;
    case "use-edit":
      return "#edit-title";
    case "use-save-device":
    case "use-cancel-edit":
      return `[data-action="use-edit"]${peer}`;
    default:
      return undefined;
  }
}

// The step of adding a device that Use shows, if it is ready.
function stepOf(v: UseView | undefined): Step["kind"] | undefined {
  return v?.kind === "ready" ? v.step.kind : undefined;
}

const stepFocus: Record<Step["kind"], string> = {
  list: '[data-action="use-add"]',
  add: "#add-title",
  config: "#config-title",
  edit: "#edit-title",
};

// The device whose line and failover Use shows for changing, if any.
function editing(v: UseView | undefined): string | undefined {
  return v?.kind === "ready" && v.step.kind === "edit" ? v.step.peerId : undefined;
}

const TAB_KEY = "homeport.tab";

function savedTab(): Tab {
  try {
    return localStorage.getItem(TAB_KEY) === "use" ? "use" : "share";
  } catch {
    return "share";
  }
}

function saveTab(t: Tab): void {
  try {
    localStorage.setItem(TAB_KEY, t);
  } catch {
    // The tab is a convenience; without storage, Share opens first.
  }
}

export function mount(signIn: SignIn, app: HTMLElement, announcer: HTMLElement, apis: HomeApis): void {
  let shownKey: string | undefined;
  let prev: State | undefined;
  let tab: Tab = savedTab();
  let share: Share | undefined;
  let identity: Identity | undefined;
  let use: Use | undefined;
  let shownShare: ShareView | undefined;
  let shownIdentity: IdentityView | undefined;
  let shownUse: UseView | undefined;
  let hungCard: string | undefined;
  let followDevices: ReturnType<typeof setInterval> | undefined;

  const homeView = (): HomeView => ({
    tab,
    share: share?.view ?? { kind: "loading" },
    identity: identity?.view,
    use: use?.view ?? { kind: "loading" },
  });

  const draw = (s: State) => {
    const key = focusKey(document.activeElement);
    const sk = screenKey(s);
    const fresh = sk !== shownKey;
    shownKey = sk;

    app.dataset.screen = s.kind;
    if (fresh) app.dataset.fresh = "";
    else delete app.dataset.fresh;
    app.innerHTML = render(s, homeView());
    // The key card hangs into place once per device, not again when the
    // screen around it is redrawn.
    const card = app.querySelector<HTMLElement>(".key-card");
    if (card) {
      if (card.dataset.peer === hungCard) card.classList.add("is-hung");
      hungCard = card.dataset.peer;
    }
    return { key, fresh };
  };

  // restoreFocus puts focus back after a redraw: on the heading if it was
  // there, on the control the member used, or, when that is gone, on what
  // it opened or on the first control of the open tab.
  const restoreFocus = (onHeading: boolean, key: FocusKey | undefined) => {
    if (onHeading) {
      focusHeading(app.querySelector<HTMLElement>("h1"));
      return;
    }
    if (!key) return;
    const fallback = fallbackFor(key);
    const target =
      app.querySelector<HTMLElement>(selectorFor(key)) ??
      (fallback ? app.querySelector<HTMLElement>(fallback) : null) ??
      app.querySelector<HTMLElement>("[data-panel] [data-action]");
    if (target?.tabIndex === -1 && target.matches("h1, h2")) focusHeading(target);
    else target?.focus();
    // A field the member was typing in keeps its caret.
    if (key.caret && target instanceof HTMLInputElement) target.setSelectionRange(...key.caret);
  };

  // redraw shows a change of one home part, in place when it can.
  const redraw = (selector: string) => {
    const s = signIn.state;
    if (s.kind !== "signedIn" && s.kind !== "signingOut") return;
    if (morph(app, render(s, homeView()), selector)) return;
    const onHeading = document.activeElement === app.querySelector("h1");
    const { key } = draw(s);
    restoreFocus(onHeading, key);
  };

  const announce = (text: string | undefined) => {
    if (text) announcer.textContent = text;
  };

  // Home runs while a member is signed in. A gone session sends the window
  // back to sign-in, which finds out who, if anyone, is signed in.
  const startHome = (identityStatus: string) => {
    identity = new Identity(apis.identity, identityStatus);
    share = new Share(apis.share, identityStatus);
    use = new Use(apis.use, identityStatus);

    identity.subscribe((v) => {
      if (v.kind === "signedOut") return goToSignIn();
      announce(identityAnnouncement(shownIdentity, v));
      shownIdentity = v;
      if (v.kind === "status" && v.state === "verified") {
        if (share?.view.kind === "verify") void share.recheck();
        void use?.unlock();
      }
      // The Account rows show the identity too.
      redraw(".home");
    });
    share.subscribe((v) => {
      if (v.kind === "signedOut") return goToSignIn();
      announce(shareAnnouncement(shownShare, v));
      shownShare = v;
      if (tab === "share") redraw("[data-share]");
    });
    use.subscribe((v) => {
      if (v.kind === "signedOut") return goToSignIn();
      const step = stepOf(shownUse);
      const edited = editing(shownUse);
      if (tab === "use") announce(useAnnouncement(shownUse, v));
      shownUse = v;
      if (tab !== "use") return;
      // The Account rows show the balance too.
      redraw(".home");
      // A new step of adding or changing a device is a new screen within
      // the tab: focus its heading, or, back at the list, the device just
      // changed or Add a device.
      const now = stepOf(v);
      if (step && now && step !== now) {
        const back = edited && now === "list" ? `[data-action="use-edit"][data-peer="${CSS.escape(edited)}"]` : "";
        const target =
          (back ? app.querySelector<HTMLElement>(back) : null) ?? app.querySelector<HTMLElement>(stepFocus[now]);
        if (target?.matches("h2")) focusHeading(target);
        else target?.focus();
      }
    });

    identity.start();
    void share.start();
    void use.start();

    // While the member looks at the device list, it follows the gateway, so
    // a device moving to a backup line shows. Forms aren't redrawn under
    // the member.
    followDevices = setInterval(() => {
      const v = use?.view;
      if (tab !== "use" || document.visibilityState === "hidden" || v?.kind !== "ready" || v.step.kind !== "list") return;
      void use?.refreshDevices();
    }, DEVICES_REFRESH_MS);
  };

  const stopHome = () => {
    clearInterval(followDevices);
    followDevices = undefined;
    share?.dispose();
    identity?.dispose();
    use?.dispose();
    share = identity = use = undefined;
    shownShare = shownIdentity = shownUse = undefined;
  };

  const goToSignIn = () => {
    stopHome();
    void signIn.boot();
  };

  const openTab = (t: Tab) => {
    const s = signIn.state;
    if (t === tab || (s.kind !== "signedIn" && s.kind !== "signingOut")) return;
    tab = t;
    saveTab(t);
    shownUse = use?.view;
    draw(s);
    app.querySelector<HTMLElement>(`[role="tab"][data-tab="${t}"]`)?.focus();
    // The balance may have moved while Use was out of sight.
    if (t === "use") void use?.refresh();
  };

  signIn.subscribe((s) => {
    const signedIn = s.kind === "signedIn" || s.kind === "signingOut";
    if (!signedIn && share) stopHome();

    const { key, fresh } = draw(s);

    const said = announcement(prev, s);
    if (said) announcer.textContent = said;
    prev = s;

    focusScreen(s, key, fresh);
    // After the focus, so the first statuses keep it where it went.
    if (signedIn && !share) startHome(s.member.identityStatus);
  });

  const focusScreen = (s: State, key: FocusKey | undefined, fresh: boolean) => {
    // The email field takes focus whenever it can be typed in, with a
    // rejected address selected so it can be retyped at once.
    const email = app.querySelector<HTMLInputElement>("#email");
    if (email && !email.readOnly) {
      email.focus();
      if (s.kind === "signedOut" && s.error) email.select();
      return;
    }
    // Same screen, new details: keep focus on the button the member used.
    // A new screen: focus its heading, so a screen reader announces it.
    restoreFocus(fresh, fresh ? undefined : key);
  };

  app.addEventListener("submit", (e) => {
    const form = (e.target as HTMLElement).closest<HTMLFormElement>("[data-form]");
    if (!form) return;
    e.preventDefault();
    const data = new FormData(form);
    switch (form.dataset.form) {
      case "email":
        return void signIn.submit(data.get("email")?.toString() ?? "");
      case "add-device": {
        // Focus stays with the button the member pressed, which the
        // config replaces; see fallbackFor.
        form.querySelector<HTMLElement>('[type="submit"]')?.focus();
        const city = data.get("city");
        if (city !== null) use?.pickCity(city.toString());
        use?.setName(data.get("name")?.toString() ?? "");
        return void use?.create();
      }
      case "edit-device": {
        // Focus stays with Save changes; back at the list it goes to the
        // device's Change button.
        form.querySelector<HTMLElement>('[type="submit"]')?.focus();
        const city = data.get("city");
        if (city !== null) use?.pickCity(city.toString());
        const failover = data.get("failover")?.toString();
        if (failover) use?.pickFailover(asFailover(failover));
        return void use?.saveDevice();
      }
    }
  });

  app.addEventListener("input", (e) => {
    const input = e.target as HTMLInputElement;
    if (input.id === "device-name") use?.setName(input.value);
    else if (input.id === "city-query") use?.setQuery(input.value);
  });

  app.addEventListener("change", (e) => {
    const input = e.target as HTMLInputElement;
    if (input.name === "city" && input.checked) use?.pickCity(input.value);
    else if (input.id === "device-country" && input.value) void use?.pickCountry(input.value);
    else if (input.name === "move" && input.checked) void use?.setMove(input.value === "move");
    else if (input.name === "failover" && input.checked) use?.pickFailover(asFailover(input.value));
  });

  // Escape leaves the form for a new or changed device, as Cancel does. In
  // the city search it clears what was typed first, as search fields do.
  app.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    const target = e.target as HTMLElement;
    if (target instanceof HTMLInputElement && target.id === "city-query" && target.value) {
      e.preventDefault();
      target.value = "";
      return use?.setQuery("");
    }
    const form = target.closest<HTMLElement>("[data-form]")?.dataset.form;
    if (form === "add-device") use?.cancelAdding();
    if (form === "edit-device") use?.cancelEditing();
  });

  // Arrow keys move between the tabs, as in any tab list.
  app.addEventListener("keydown", (e) => {
    const t = (e.target as HTMLElement).closest<HTMLElement>('[role="tab"]');
    if (!t) return;
    const order: Tab[] = ["share", "use"];
    const i = order.indexOf(t.dataset.tab as Tab);
    let next: Tab | undefined;
    if (e.key === "ArrowRight") next = order[(i + 1) % order.length];
    else if (e.key === "ArrowLeft") next = order[(i + order.length - 1) % order.length];
    else if (e.key === "Home") next = order[0];
    else if (e.key === "End") next = order[order.length - 1];
    if (!next) return;
    e.preventDefault();
    openTab(next);
  });

  app.addEventListener("click", (e) => {
    const button = (e.target as HTMLElement).closest<HTMLElement>("[data-action]");
    if (!button || button.getAttribute("aria-disabled") === "true") return;
    const peer = button.dataset.peer ?? "";
    switch (button.dataset.action) {
      case "resend":
        return void signIn.resend();
      case "change-email":
        return void signIn.useDifferentEmail();
      case "retry":
        return void signIn.boot();
      case "sign-out":
        return void signIn.signOut();
      case "tab":
        return openTab(button.dataset.tab === "use" ? "use" : "share");
      case "share-toggle":
        return void share?.toggle();
      case "share-recheck":
        return void share?.recheck();
      case "identity-verify":
        return void identity?.verify();
      case "identity-check":
        return void identity?.check();
      case "use-unlock":
        return void use?.unlock();
      case "use-retry":
        return void use?.retry();
      case "use-buy":
        return void use?.buy();
      case "use-check-payment":
        return void use?.checkPayment();
      case "use-add":
        return void use?.startAdding();
      case "use-reload-regions":
        return void use?.reloadRegions();
      case "use-reload-cities":
        return void use?.reloadCities();
      case "use-cancel-add":
        return use?.cancelAdding();
      case "use-save":
        return void use?.save();
      case "use-done":
        return void use?.done();
      case "use-remove":
        return use?.askRemove(peer);
      case "use-cancel-remove":
        return use?.cancelRemove();
      case "use-confirm-remove":
        return void use?.confirmRemove();
      case "use-edit":
        return use?.startEditing(peer);
      case "use-cancel-edit":
        return use?.cancelEditing();
      case "use-https-only":
        return void use?.toggleHttpsOnly(peer);
      case "use-form-https-only":
        return use?.pickHttpsOnly(button.getAttribute("aria-checked") !== "true");
    }
  });
}

function focusHeading(h: HTMLElement | null): void {
  if (!h) return;
  h.tabIndex = -1;
  h.focus({ preventScroll: true });
}
