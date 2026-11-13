import "@fontsource/barlow-semi-condensed/latin-500.css";
import "@fontsource/barlow-semi-condensed/latin-600.css";
import "./style.css";

import { SignIn, type AuthApi } from "./signin";
import { mount } from "./ui";

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
  const signIn = new SignIn(await authApi());
  mount(signIn, document.getElementById("app")!, document.getElementById("announcer")!);
  await signIn.boot();
}

void main();
