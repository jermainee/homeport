// Connects the sign-in and Share state machines to the window: renders each
// state, handles the buttons and the form, moves focus and announces changes.

import { Share, type ShareApi, type ShareView } from "./share";
import type { SignIn, State } from "./signin";
import { render, shareLine } from "./view";

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

// morphShare updates the Share part of the window in place when its markup
// keeps its shape. The switch then slides, the window light fades, and
// focus, selection and a screen reader's place survive the updates Go sends
// every second while sharing.
function morphShare(app: HTMLElement, html: string): boolean {
  const live = app.querySelector("[data-share]");
  if (!live) return false;
  const tpl = document.createElement("template");
  tpl.innerHTML = html;
  const next = tpl.content.querySelector("[data-share]");
  if (!next || !sameShape(live, next)) return false;
  sync(live, next);
  return true;
}

export function mount(signIn: SignIn, app: HTMLElement, announcer: HTMLElement, shareApi: ShareApi): void {
  let shownKey: string | undefined;
  let prev: State | undefined;
  let share: Share | undefined;
  let shownShare: ShareView | undefined;

  const draw = (s: State) => {
    const action = (document.activeElement as HTMLElement | null)?.dataset.action;
    const key = screenKey(s);
    const fresh = key !== shownKey;
    shownKey = key;
    shownShare = share?.view;

    app.dataset.screen = s.kind;
    if (fresh) app.dataset.fresh = "";
    else delete app.dataset.fresh;
    app.innerHTML = render(s, share?.view);
    return { action, fresh };
  };

  // Share runs while a member is signed in. A gone session sends the window
  // back to sign-in, which finds out who, if anyone, is signed in.
  const startShare = (identity: string) => {
    share = new Share(shareApi, identity);
    share.subscribe((v) => {
      if (v.kind === "signedOut") {
        stopShare();
        void signIn.boot();
        return;
      }
      const said = shareAnnouncement(shownShare, v);
      if (said) announcer.textContent = said;
      const s = signIn.state;
      if (s.kind !== "signedIn" && s.kind !== "signingOut") return;
      shownShare = v;
      if (morphShare(app, render(s, v))) return;
      const { action } = draw(s);
      if (action) app.querySelector<HTMLElement>(`[data-action="${action}"]`)?.focus();
    });
    void share.start();
  };
  const stopShare = () => {
    share?.dispose();
    share = undefined;
    shownShare = undefined;
  };

  signIn.subscribe((s) => {
    const signedIn = s.kind === "signedIn" || s.kind === "signingOut";
    if (!signedIn && share) stopShare();

    const { action, fresh } = draw(s);
    // After the draw, so home counts as a new screen for focus below.
    if (signedIn && !share) queueMicrotask(() => startShare(s.member.identityStatus));

    const said = announcement(prev, s);
    if (said) announcer.textContent = said;
    prev = s;

    // The email field takes focus whenever it can be typed in, with a
    // rejected address selected so it can be retyped at once.
    const email = app.querySelector<HTMLInputElement>("#email");
    if (email && !email.readOnly) {
      email.focus();
      if (s.kind === "signedOut" && s.error) email.select();
      return;
    }
    if (!fresh) {
      // Same screen, new details: keep focus on the button the member used.
      if (action) app.querySelector<HTMLElement>(`[data-action="${action}"]`)?.focus();
      return;
    }
    // A new screen: focus its heading, so a screen reader announces it.
    const title = app.querySelector<HTMLElement>("h1");
    if (title) {
      title.tabIndex = -1;
      title.focus({ preventScroll: true });
    }
  });

  app.addEventListener("submit", (e) => {
    const form = (e.target as HTMLElement).closest<HTMLFormElement>("[data-form=email]");
    if (!form) return;
    e.preventDefault();
    void signIn.submit(new FormData(form).get("email")?.toString() ?? "");
  });

  app.addEventListener("click", (e) => {
    const button = (e.target as HTMLElement).closest<HTMLElement>("[data-action]");
    if (!button || button.getAttribute("aria-disabled") === "true") return;
    switch (button.dataset.action) {
      case "resend":
        return void signIn.resend();
      case "change-email":
        return void signIn.useDifferentEmail();
      case "retry":
        return void signIn.boot();
      case "sign-out":
        return void signIn.signOut();
      case "share-toggle":
        return void share?.toggle();
      case "share-recheck":
        return void share?.recheck();
    }
  });
}
