// Connects the sign-in state machine to the window: renders each state,
// handles the buttons and the form, moves focus and announces changes.

import type { SignIn, State } from "./signin";
import { render } from "./view";

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

export function mount(signIn: SignIn, app: HTMLElement, announcer: HTMLElement): void {
  let shownKey: string | undefined;
  let prev: State | undefined;

  signIn.subscribe((s) => {
    const action = (document.activeElement as HTMLElement | null)?.dataset.action;
    const key = screenKey(s);
    const fresh = key !== shownKey;
    shownKey = key;

    app.dataset.screen = s.kind;
    if (fresh) app.dataset.fresh = "";
    else delete app.dataset.fresh;
    app.innerHTML = render(s);

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
    }
  });
}
