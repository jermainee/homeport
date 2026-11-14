import "@fontsource/barlow-semi-condensed/latin-500.css";
import "@fontsource/barlow-semi-condensed/latin-600.css";
import "./style.css";

import { SignIn, type AuthApi } from "./signin";
import { mount, type HomeApis } from "./ui";

// In `npm run dev`, ?preview=<state> runs the screens against fake Go
// services, so they can be designed in a plain browser; &share=<state> and
// &use=<state> pick Share's and Use's. Production builds drop this branch.
async function services(): Promise<{ auth: AuthApi; home: HomeApis }> {
  const params = new URLSearchParams(location.search);
  const preview = params.get("preview");
  if (import.meta.env.DEV && preview !== null) {
    const { previewAuth, previewShare, previewIdentity, previewUse } = await import("./preview");
    const tab = params.get("tab");
    if (tab) localStorage.setItem("homeport.tab", tab);
    return {
      auth: previewAuth(preview),
      home: {
        share: previewShare(params.get("share") ?? "off"),
        identity: previewIdentity(),
        use: previewUse(params.get("use") ?? "ready"),
      },
    };
  }
  const [{ wailsAuth }, { wailsShare }, { wailsIdentity }, { wailsUse }] = await Promise.all([
    import("./auth"),
    import("./share-api"),
    import("./identity-api"),
    import("./use-api"),
  ]);
  return { auth: wailsAuth, home: { share: wailsShare, identity: wailsIdentity, use: wailsUse } };
}

async function main(): Promise<void> {
  const { auth, home } = await services();
  const signIn = new SignIn(auth);
  mount(signIn, document.getElementById("app")!, document.getElementById("announcer")!, home);
  await signIn.boot();
}

void main();
