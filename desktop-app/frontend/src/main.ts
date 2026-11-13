import "@fontsource/barlow-semi-condensed/latin-500.css";
import "@fontsource/barlow-semi-condensed/latin-600.css";
import "./style.css";

import { SignIn, type AuthApi, type State } from "./signin";
import { render } from "./view";

// In `npm run dev`, ?preview=<state> runs the screens against a fake
// AuthService, so they can be designed in a plain browser. Production
// builds drop this branch.
async function authApi(): Promise<AuthApi> {
  const preview = new URLSearchParams(location.search).get("preview");
  if (import.meta.env.DEV && preview !== null) {
    const { previewAuth } = await import("./preview");
    return previewAuth(preview);
  }
  const { wailsAuth } = await import("./auth");
  return wailsAuth;
}

async function main(): Promise<void> {
  const app = document.getElementById("app")!;
  const announcer = document.getElementById("announcer")!;
  const signIn = new SignIn(await authApi());
  let shown: State["kind"] | undefined;
  let offline = false;

  signIn.subscribe((s) => {
    const action = (document.activeElement as HTMLElement | null)?.dataset.action;
    app.dataset.screen = s.kind;
    app.innerHTML = render(s);

    // The screen is re-rendered on every change, so a live region inside it
    // would be new each time; this one outlives the screens.
    const nowOffline = s.kind === "waiting" && s.offline;
    if (nowOffline !== offline && s.kind === "waiting") {
      announcer.textContent = nowOffline
        ? "Homeport can't be reached. Trying again."
        : "Connected again. Waiting for you to confirm.";
    }
    offline = nowOffline;

    if (s.kind === shown) {
      // Same screen, new details: keep focus on the button the member used.
      if (action) app.querySelector<HTMLElement>(`[data-action="${action}"]`)?.focus();
      return;
    }
    shown = s.kind;
    // A new screen: focus where the member acts next, or the heading, so a
    // screen reader announces the change.
    const email = app.querySelector<HTMLInputElement>("#email");
    if (email && !email.readOnly) {
      email.focus();
      if (s.kind === "signedOut" && s.error) email.select();
      return;
    }
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
    const action = (e.target as HTMLElement).closest<HTMLElement>("[data-action]")?.dataset.action;
    switch (action) {
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

  await signIn.boot();
}

void main();
