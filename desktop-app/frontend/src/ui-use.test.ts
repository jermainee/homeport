// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SignIn } from "./signin";
import { FakeAuth, member, serviceError } from "./test/fake-auth";
import { FakeIdentity } from "./test/fake-identity";
import { FakeShare } from "./test/fake-share";
import { FakeUse, laptop, lyon } from "./test/fake-use";
import { countryName } from "./text";
import { mount } from "./ui";
import { DEVICES_REFRESH_MS } from "./use";

let auth: FakeAuth;
let use: FakeUse;
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
  use = new FakeUse();
  identity = new FakeIdentity();
  signIn = new SignIn(auth);
  mount(signIn, app, announcer, { share: new FakeShare(), identity, use });
  await signIn.boot();
}

const q = <T extends HTMLElement = HTMLElement>(sel: string) => app.querySelector<T>(sel);

async function openUse() {
  q('[role="tab"][data-tab="use"]')!.click();
  await vi.waitFor(() => expect(q(".use-panel[aria-busy]")).toBeNull());
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  signIn.dispose();
});

describe("the tabs", () => {
  beforeEach(async () => {
    await openHome();
  });

  it("open Use, and remember it for the next start", async () => {
    await openUse();

    expect(q('[data-tab="use"]')?.getAttribute("aria-selected")).toBe("true");
    expect(q('[data-tab="share"]')?.getAttribute("tabindex")).toBe("-1");
    expect(q("[data-use]")).not.toBeNull();
    expect(q("[data-share]")).toBeNull();
    expect(document.activeElement).toBe(q('[data-tab="use"]'));

    signIn.dispose();
    await openHome();
    await vi.waitFor(() => expect(q(".use-panel")).not.toBeNull());
  });

  it("move with the arrow keys", async () => {
    const shareTab = q('[data-tab="share"]')!;
    shareTab.focus();

    shareTab.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowRight", bubbles: true }));

    expect(q('[data-tab="use"]')?.getAttribute("aria-selected")).toBe("true");
    expect(document.activeElement).toBe(q('[data-tab="use"]'));
  });

  it("refresh Use's balance when it opens", async () => {
    await openUse();
    const loads = use.count("balance");

    q('[data-tab="share"]')!.click();
    q('[data-tab="use"]')!.click();

    await vi.waitFor(() => expect(use.count("balance")).toBe(loads + 1));
  });
});

describe("the Use tab", () => {
  beforeEach(async () => {
    await openHome();
    await openUse();
  });

  it("shows the credits as bought and earned, and what they're enough for", () => {
    expect(q('[data-field="purchased"]')?.textContent).toBe("25,000");
    expect(q('[data-field="earned"]')?.textContent).toBe("5,000");
    expect(q("#use-line")?.textContent).toBe("30,000 credits, enough for about 3 GB of use.");
  });

  it("walks focus through adding a device", async () => {
    q('[data-action="use-add"]')!.click();
    await vi.waitFor(() => expect(document.activeElement).toBe(q("#add-title")));

    q<HTMLInputElement>(`input[name="line"][value="${lyon.lineId}"]`)!.click();
    const name = q<HTMLInputElement>("#device-name")!;
    name.value = "iPhone";
    name.dispatchEvent(new Event("input", { bubbles: true }));
    q<HTMLFormElement>('[data-form="add-device"]')!.requestSubmit();

    await vi.waitFor(() => expect(document.activeElement).toBe(q("#config-title")));
    expect(use.calls).toContain(`addDevice ${lyon.lineId} iPhone`);
    expect(q<HTMLImageElement>(".key-card img")?.getAttribute("src")).toMatch(/^data:image\/png;base64,/);

    q('[data-action="use-done"]')!.click();
    await vi.waitFor(() => expect(document.activeElement).toBe(q('[data-action="use-add"]')));
    expect(use.forgotten).toEqual(["peer-3"]);
  });

  it("leaves the form on Escape", async () => {
    q('[data-action="use-add"]')!.click();
    await vi.waitFor(() => expect(q("#device-name")).not.toBeNull());

    q("#device-name")!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));

    await vi.waitFor(() => expect(document.activeElement).toBe(q('[data-action="use-add"]')));
  });

  it("hangs the key card once, not again when the config is saved", async () => {
    q('[data-action="use-add"]')!.click();
    await vi.waitFor(() => expect(q("#device-name")).not.toBeNull());
    const name = q<HTMLInputElement>("#device-name")!;
    name.value = "iPhone";
    name.dispatchEvent(new Event("input", { bubbles: true }));
    q<HTMLFormElement>('[data-form="add-device"]')!.requestSubmit();
    await vi.waitFor(() => expect(q(".key-card")).not.toBeNull());
    expect(q(".key-card")?.classList.contains("is-hung")).toBe(false);

    q('[data-action="use-save"]')!.click();

    await vi.waitFor(() => expect(q(".status.is-done")?.textContent).toContain("Saved."));
    expect(q(".key-card")?.classList.contains("is-hung")).toBe(true);
    expect(announcer.textContent).toBe("Saved.");
  });

  it("keeps a typed name while the line changes", async () => {
    q('[data-action="use-add"]')!.click();
    await vi.waitFor(() => expect(q("#device-name")).not.toBeNull());
    const name = q<HTMLInputElement>("#device-name")!;
    name.value = "Travel router";
    name.dispatchEvent(new Event("input", { bubbles: true }));

    const radio = q<HTMLInputElement>(`input[name="line"][value="${lyon.lineId}"]`)!;
    radio.click();
    radio.dispatchEvent(new Event("change", { bubbles: true }));

    expect(q<HTMLInputElement>("#device-name")?.value).toBe("Travel router");
  });

  it("asks before removing a device, with focus on Keep", async () => {
    q(`[data-action="use-remove"][data-peer="${laptop.peerId}"]`)!.focus();
    q(`[data-action="use-remove"][data-peer="${laptop.peerId}"]`)!.click();

    await vi.waitFor(() => expect(document.activeElement).toBe(q('[data-action="use-cancel-remove"]')));
    q('[data-action="use-cancel-remove"]')!.click();

    await vi.waitFor(() => expect(document.activeElement).toBe(q('[data-action="use-remove"]')));
    expect(use.removed).toEqual([]);
  });

  it("says when a device is removed", async () => {
    q('[data-action="use-remove"]')!.click();
    await vi.waitFor(() => expect(q('[data-action="use-confirm-remove"]')).not.toBeNull());

    // A keyboard user: Enter on the focused button.
    q('[data-action="use-confirm-remove"]')!.focus();
    q('[data-action="use-confirm-remove"]')!.click();

    await vi.waitFor(() => expect(announcer.textContent).toBe("Device removed."));
    expect(q(".devices")).toBeNull();
    expect(document.activeElement).toBe(q('[data-action="use-add"]'));
  });
});

describe("a device on a backup line", () => {
  const potsdam = { lineId: "line-potsdam", country: "DE", city: "Potsdam", since: "2026-11-14T13:02:10Z" };

  it("says its line went offline, and where it runs instead", async () => {
    await openHome();
    use.devicesAnswer = [{ ...laptop, exitLine: potsdam }];
    await openUse();

    const backup = q(`.device[data-peer="${laptop.peerId}"] .device-backup`);
    expect(backup?.textContent?.trim()).toBe(
      `Its line went offline, so it's using a backup line in Potsdam, ${countryName("DE")}.`,
    );
  });

  it("says less when Homeport doesn't know where the backup line is", async () => {
    await openHome();
    use.devicesAnswer = [{ ...laptop, exitLine: { ...potsdam, country: "", city: "" } }];
    await openUse();

    expect(q(".device-backup")?.textContent?.trim()).toBe("Its line went offline, so it's using a backup line.");
  });

  it.each([
    ["has no session", null],
    ["runs on its own line", { ...potsdam, lineId: laptop.lineId }],
  ])("says nothing more while the device %s", async (_, exitLine) => {
    await openHome();
    use.devicesAnswer = [{ ...laptop, exitLine }];
    await openUse();

    expect(q(".device")).not.toBeNull();
    expect(q(".device-backup")).toBeNull();
  });

  it("follows the devices while Use is open", async () => {
    // The app's own timer, run by hand: fake clocks upset happy-dom.
    const timers = vi.spyOn(globalThis, "setInterval");
    await openHome();
    await openUse();
    const follow = timers.mock.calls.find(([, ms]) => ms === DEVICES_REFRESH_MS)?.[0] as () => void;
    timers.mockRestore();
    use.devicesAnswer = [{ ...laptop, exitLine: potsdam }];
    const lines = use.count("lines");

    follow();

    await vi.waitFor(() => expect(q(".device-backup")).not.toBeNull());
    // Only the devices: the lines are fetched as before.
    expect(use.count("lines")).toBe(lines);
  });

  it("doesn't redraw a form under the member", async () => {
    const timers = vi.spyOn(globalThis, "setInterval");
    await openHome();
    await openUse();
    const follow = timers.mock.calls.find(([, ms]) => ms === DEVICES_REFRESH_MS)?.[0] as () => void;
    timers.mockRestore();
    q(`[data-action="use-edit"][data-peer="${laptop.peerId}"]`)!.click();
    const devices = use.count("devices");

    follow();

    expect(use.count("devices")).toBe(devices);
  });
});

describe("changing a device", () => {
  beforeEach(async () => {
    await openHome();
    await openUse();
  });

  const openChange = async () => {
    q(`[data-action="use-edit"][data-peer="${laptop.peerId}"]`)!.click();
    await vi.waitFor(() => expect(document.activeElement).toBe(q("#edit-title")));
  };

  it("walks focus through it, and back to the device", async () => {
    q(`[data-action="use-edit"][data-peer="${laptop.peerId}"]`)!.focus();
    await openChange();
    expect(q<HTMLInputElement>(`input[name="line"][value="${laptop.lineId}"]`)?.checked).toBe(true);
    expect(q<HTMLInputElement>('input[name="failover"][value="country"]')?.checked).toBe(true);

    q<HTMLInputElement>(`input[name="line"][value="${lyon.lineId}"]`)!.click();
    q<HTMLInputElement>('input[name="failover"][value="off"]')!.click();
    q<HTMLFormElement>('[data-form="edit-device"]')!.requestSubmit();

    await vi.waitFor(() => expect(announcer.textContent).toBe("Changes saved."));
    expect(use.updates).toEqual([[laptop.peerId, { lineId: lyon.lineId, failover: "off" }]]);
    expect(document.activeElement).toBe(q(`[data-action="use-edit"][data-peer="${laptop.peerId}"]`));
  });

  it("names the backup choices after the picked line", async () => {
    await openChange();
    const choices = () => [...app.querySelectorAll(".failover-option")].map((o) => o.textContent?.replace(/\s+/g, " ").trim());
    expect(choices()[0]).toBe(`Use another line in Berlin, or else in ${countryName("DE")}`);
    expect(choices()[1]).toBe("Use another line in Berlin only");

    const radio = q<HTMLInputElement>(`input[name="line"][value="${lyon.lineId}"]`)!;
    radio.click();
    radio.dispatchEvent(new Event("change", { bubbles: true }));

    await vi.waitFor(() => expect(choices()[1]).toBe("Use another line in Lyon only"));
  });

  it("leaves on Escape, with focus back on the device", async () => {
    await openChange();

    q('input[name="failover"]')!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));

    await vi.waitFor(() => expect(document.activeElement).toBe(q(`[data-action="use-edit"][data-peer="${laptop.peerId}"]`)));
    expect(use.updates).toEqual([]);
  });

  it("says why the change didn't save", async () => {
    await openChange();
    use.updateAnswer = serviceError("line_on_other_gateway");

    q<HTMLInputElement>(`input[name="line"][value="${lyon.lineId}"]`)!.click();
    q<HTMLFormElement>('[data-form="edit-device"]')!.requestSubmit();

    await vi.waitFor(() => expect(q('.use-step [role="alert"]')?.textContent).toContain("another Homeport gateway"));
    expect(q("#edit-title")).not.toBeNull();
  });
});

describe("buying credits", () => {
  it("keeps Buy credits available while waiting, for a member who left Checkout", async () => {
    await openHome();
    await openUse();

    q('[data-action="use-buy"]')!.click();

    await vi.waitFor(() => expect(q(".panel-actions .status")?.textContent).toContain("Finish paying"));
    expect(q('[data-action="use-buy"]')?.hasAttribute("aria-disabled")).toBe(false);
  });
});

describe("adding a device when no line is shared", () => {
  it("says so, and offers no config to create", async () => {
    await openHome();
    use.linesAnswer = [];
    await openUse();

    q('[data-action="use-add"]')!.click();

    await vi.waitFor(() => expect(q("#add-title")).not.toBeNull());
    expect(q(".use-step .empty")?.textContent).toContain("Nobody shares a line");
    expect(q('[data-focus="use-create"]')).toBeNull();
    expect(q("#device-name")).toBeNull();
  });
});

describe("the Account rows", () => {
  it("follow the balance after credits arrive", async () => {
    await openHome();
    await openUse();
    use.checkoutPolls = [{ arrived: true, balance: { purchased: 1_025_000, earned: 5_000, total: 1_030_000, bytes: 0 } }];

    q('[data-action="use-buy"]')!.click();

    await vi.waitFor(() => expect(announcer.textContent).toBe("Your credits arrived."), { timeout: 5000 });
    expect(q(".facts .row:last-child dd")?.textContent).toBe("1,030,000");
  });
});

describe("for a member who isn't verified", () => {
  it("offers the identity check on the Use tab too, and opens Use once verified", async () => {
    await openHome("unverified");
    await openUse();
    expect(q("#use-line")?.textContent).toBe("Verify your identity first");
    identity.startAnswer = "verified";

    q('[data-action="identity-verify"]')!.click();

    await vi.waitFor(() => expect(q('[data-field="purchased"]')).not.toBeNull());
  });
});

describe("text from the API", () => {
  it("shows a device name as text, never as markup", async () => {
    await openHome();
    use.devicesAnswer = [{ ...laptop, name: '<img src=x onerror="alert(1)">' }];
    await openUse();

    expect(q(".devices img")).toBeNull();
    expect(q(".device h3")?.textContent).toBe('<img src=x onerror="alert(1)">');
  });
});
