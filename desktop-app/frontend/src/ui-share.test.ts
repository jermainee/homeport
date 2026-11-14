// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SignIn } from "./signin";
import { FakeAuth, member } from "./test/fake-auth";
import { FakeIdentity } from "./test/fake-identity";
import { FakeShare } from "./test/fake-share";
import { FakeUse } from "./test/fake-use";
import { mount } from "./ui";

let auth: FakeAuth;
let share: FakeShare;
let identity: FakeIdentity;
let signIn: SignIn;
let app: HTMLElement;
let announcer: HTMLElement;

async function openHome(identityStatus = "verified") {
  document.body.innerHTML = `<main id="app"></main><p id="announcer" role="status"></p>`;
  app = document.getElementById("app")!;
  announcer = document.getElementById("announcer")!;
  auth = new FakeAuth();
  auth.signedIn = { ...member, identityStatus };
  share = new FakeShare();
  signIn = new SignIn(auth);
  identity = new FakeIdentity();
  mount(signIn, app, announcer, { share, identity, use: new FakeUse() });
  await signIn.boot();
  await vi.waitFor(() => expect(app.querySelector(".share-panel[aria-busy]")).toBeNull());
}

const toggle = () => app.querySelector<HTMLButtonElement>('[data-action="share-toggle"]')!;

afterEach(() => {
  signIn.dispose();
});

describe("the Share panel", () => {
  beforeEach(async () => {
    await openHome();
  });

  it("keeps focus on the switch while Share turns on and goes online", async () => {
    const sw = toggle();
    sw.focus();
    expect(sw.getAttribute("aria-checked")).toBe("false");

    sw.click();
    await vi.waitFor(() => expect(toggle().hasAttribute("aria-busy")).toBe(false));
    expect(share.toggles).toEqual([true]);
    share.emit({ state: "online", sharing: true, bytesToday: 2_500_000, flowsActive: 1 });

    await vi.waitFor(() => expect(app.querySelector(".house-window")?.getAttribute("data-light")).toBe("on"));
    expect(toggle().getAttribute("aria-checked")).toBe("true");
    expect(document.activeElement).toBe(toggle());
    expect(app.querySelector('[data-field="bytesToday"]')?.textContent).toBe("2.5 MB");
  });

  it("keeps focus on the home heading while Share's status arrives", () => {
    expect(document.activeElement).toBe(app.querySelector("h1"));
  });

  it("updates today's count in place", async () => {
    share.emit({ state: "online", sharing: true, bytesToday: 1_000_000 });
    const count = app.querySelector('[data-field="bytesToday"]');

    share.emit({ bytesToday: 3_400_000 });

    expect(app.querySelector('[data-field="bytesToday"]')).toBe(count);
    expect(count?.textContent).toBe("3.4 MB");
  });

  it("says when another VPN pauses Share", () => {
    share.emit({ state: "online", sharing: true });

    share.emit({ state: "paused" });

    expect(announcer.textContent).toContain("Paused while another VPN is on");
    expect(app.querySelector("#share-line")?.textContent).toContain("Paused while another VPN is on");
  });

  it("goes back to sign-in when the session is gone", async () => {
    auth.signedIn = null;

    share.emit({ state: "error", sharing: false, error: "signed_out" });

    await vi.waitFor(() => expect(app.dataset.screen).toBe("signedOut"));
    expect(share.listening).toBe(0);
  });
});

describe("for a member who isn't verified", () => {
  beforeEach(async () => {
    await openHome("unverified");
  });

  it("asks them to verify instead of showing a switch", () => {
    expect(app.querySelector('[role="switch"]')).toBeNull();
    expect(app.querySelector("#share-line")?.textContent).toBe("Verify your identity first");
    expect(app.querySelector('[data-action="identity-verify"]')?.textContent?.trim()).toBe("Verify identity");
  });

  it("shows the switch once the check finds them verified, and focuses it", async () => {
    identity.startAnswer = "verified";
    const verify = app.querySelector<HTMLElement>('[data-action="identity-verify"]')!;
    verify.focus();
    verify.click();

    await vi.waitFor(() => expect(app.querySelector('[role="switch"]')).not.toBeNull());
    expect(identity.starts).toBe(1);
    expect(share.identityChecks).toBe(1);
    expect(document.activeElement).toBe(app.querySelector('[role="switch"]'));
  });

  it("follows the check in the browser", async () => {
    const verify = app.querySelector<HTMLElement>('[data-action="identity-verify"]')!;
    verify.focus();
    verify.click();

    await vi.waitFor(() =>
      expect(app.querySelector("#share-line")?.textContent).toBe("Finish the check in your browser"),
    );
    expect(announcer.textContent).toBe("Finish the check in your browser.");
    // A member who closed Stripe's page can open a new check at once.
    expect(app.querySelector('[data-action="identity-verify"]')?.textContent?.trim()).toBe("Start a new check");
  });
});
