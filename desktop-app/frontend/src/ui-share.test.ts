// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SignIn } from "./signin";
import { FakeAuth, member } from "./test/fake-auth";
import { FakeShare } from "./test/fake-share";
import { mount } from "./ui";

let auth: FakeAuth;
let share: FakeShare;
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
  mount(signIn, app, announcer, share);
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
    expect(app.querySelector('[data-action="share-recheck"]')?.textContent?.trim()).toBe("Check again");
  });

  it("shows the switch once a check finds them verified", async () => {
    app.querySelector<HTMLElement>('[data-action="share-recheck"]')!.click();

    await vi.waitFor(() => expect(app.querySelector('[role="switch"]')).not.toBeNull());
    expect(share.identityChecks).toBe(1);
  });
});
