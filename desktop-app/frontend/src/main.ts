import "@fontsource/barlow-semi-condensed/latin-500.css";
import "@fontsource/barlow-semi-condensed/latin-600.css";
import "./style.css";

import type { ShareApi } from "./share";
import { SignIn, type AuthApi } from "./signin";
import { mount } from "./ui";

// In `npm run dev`, ?preview=<state> runs the screens against a fake
// AuthService and ShareService, so they can be designed in a plain browser;
// &share=<state> picks Share's. Production builds drop this branch.
async function services(): Promise<{ auth: AuthApi; share: ShareApi }> {
  const params = new URLSearchParams(location.search);
  const preview = params.get("preview");
  if (import.meta.env.DEV && preview !== null) {
    const { previewAuth, previewShare } = await import("./preview");
    return { auth: previewAuth(preview), share: previewShare(params.get("share") ?? "off") };
  }
  const [{ wailsAuth }, { wailsShare }] = await Promise.all([import("./auth"), import("./share-api")]);
  return { auth: wailsAuth, share: wailsShare };
}

async function main(): Promise<void> {
  const { auth, share } = await services();
  const signIn = new SignIn(auth);
  mount(signIn, document.getElementById("app")!, document.getElementById("announcer")!, share);
  await signIn.boot();
}

void main();
