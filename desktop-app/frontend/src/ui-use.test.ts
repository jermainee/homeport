// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SignIn } from "./signin";
import { FakeAuth, member, serviceError } from "./test/fake-auth";
import { FakeIdentity } from "./test/fake-identity";
import { FakeShare } from "./test/fake-share";
import { FakeUse, laptop } from "./test/fake-use";
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
const qa = (sel: string) => [...app.querySelectorAll<HTMLElement>(sel)];

/** Opens Add a device and waits until the picked country's cities are shown. */
async function startAdding() {
  q('[data-action="use-add"]')!.click();
  await vi.waitFor(() => expect(q('input[name="city"]')).not.toBeNull());
}

function pick(value: string) {
  const radio = q<HTMLInputElement>(`input[name="city"][value="${value}"]`)!;
  radio.click();
  radio.dispatchEvent(new Event("change", { bubbles: true }));
}

function type(sel: string, text: string) {
  const input = q<HTMLInputElement>(sel)!;
  input.focus();
  input.value = text;
  input.dispatchEvent(new Event("input", { bubbles: true }));
}

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

  it("walks focus through adding a device, and says where its line is", async () => {
    q('[data-action="use-add"]')!.click();
    await vi.waitFor(() => expect(document.activeElement).toBe(q("#add-title")));
    await vi.waitFor(() => expect(q('input[name="city"]')).not.toBeNull());
    // The countries arriving don't take focus off the heading.
    expect(document.activeElement).toBe(q("#add-title"));

    pick("Munich");
    type("#device-name", "iPhone");
    q<HTMLFormElement>('[data-form="add-device"]')!.requestSubmit();

    await vi.waitFor(() => expect(document.activeElement).toBe(q("#config-title")));
    expect(use.calls).toContain("addDevice DE Munich iPhone");
    expect(q<HTMLImageElement>(".key-card img")?.getAttribute("src")).toMatch(/^data:image\/png;base64,/);
    expect(q(".key-place")?.textContent).toBe("Munich, Germany");
    expect(q(".key-isp")?.textContent).toBe("Vodafone line");

    q('[data-action="use-done"]')!.click();
    await vi.waitFor(() => expect(document.activeElement).toBe(q('[data-action="use-add"]')));
    expect(use.forgotten).toEqual(["peer-3"]);
  });

  it("offers the countries with their free lines, and any city of the picked one first", async () => {
    await startAdding();

    const options = qa("#device-country option").map((o) => o.textContent?.trim());
    expect(options).toEqual(["France, 1 free line", "Germany, 4 free lines"]);
    expect(q<HTMLSelectElement>("#device-country")?.value).toBe("DE");
    const cities = qa(".city-option").map((o) => o.textContent?.replace(/\s+/g, " ").trim());
    expect(cities).toEqual(["Any city in Germany 4 free", "Berlin 2 free", "Munich 1 free"]);
    expect(q<HTMLInputElement>('input[name="city"][value=""]')?.checked).toBe(true);
  });

  it("shows another country's cities when the member picks it", async () => {
    await startAdding();

    const select = q<HTMLSelectElement>("#device-country")!;
    select.value = "FR";
    select.dispatchEvent(new Event("change", { bubbles: true }));

    await vi.waitFor(() => expect(q('input[name="city"][value="Lyon"]')).not.toBeNull());
    expect(q(".city-option .line-place")?.textContent).toBe("Any city in France");
  });

  it("finds a city by name when a country has more than the list shows", async () => {
    signIn.dispose();
    await openHome();
    const many = ["Aachen", "Berlin", "Bonn", "Bremen", "Cologne", "Dresden", "Essen", "Hamburg", "Hanover", "Leipzig"];
    use.countriesAnswer = [{ country: "DE", lines: 20 }];
    use.citiesAnswers = { DE: many.map((city) => ({ city, lines: 2 })) };
    await openUse();
    await startAdding();
    expect(qa('input[name="city"]').length).toBe(1 + 8);
    expect(q(".city-more")?.textContent).toBe("2 more cities. Type to find one.");
    expect(q("#city-query")?.getAttribute("aria-describedby")).toBe("city-more");

    type("#city-query", "ha");

    await vi.waitFor(() => expect(qa('input[name="city"]').length).toBe(1 + 2));
    expect(qa(".city-option .line-place").map((p) => p.textContent)).toEqual(["Any city in Germany", "Hamburg", "Hanover"]);
    expect(document.activeElement).toBe(q("#city-query"));
    expect(q<HTMLInputElement>("#city-query")?.selectionStart).toBe(2);
  });

  it("keeps focus on a city the member picks with the keyboard, also when an error goes away", async () => {
    await startAdding();
    pick("Munich");
    type("#device-name", "iPhone");
    use.addAnswer = serviceError("no_free_line");
    q<HTMLFormElement>('[data-form="add-device"]')!.requestSubmit();
    await vi.waitFor(() => expect(q('[role="alert"]')).not.toBeNull());

    q<HTMLInputElement>('input[name="city"][value="Berlin"]')!.focus();
    pick("Berlin");

    await vi.waitFor(() => expect(q('[role="alert"]')).toBeNull());
    expect(document.activeElement).toBe(q('input[name="city"][value="Berlin"]'));
  });

  it("offers to load a country's cities again when they didn't load", async () => {
    signIn.dispose();
    await openHome();
    use.citiesAnswers = { DE: serviceError("offline") };
    await openUse();
    q('[data-action="use-add"]')!.click();
    await vi.waitFor(() => expect(q('[data-action="use-reload-cities"]')).not.toBeNull());
    use.citiesAnswers = { DE: [{ city: "Berlin", lines: 2 }] };

    q('[data-action="use-reload-cities"]')!.click();

    await vi.waitFor(() => expect(q('input[name="city"][value="Berlin"]')).not.toBeNull());
  });

  it("says when a region has no free line any more", async () => {
    await startAdding();
    pick("Munich");
    type("#device-name", "iPhone");
    use.addAnswer = serviceError("no_free_line");

    q<HTMLFormElement>('[data-form="add-device"]')!.requestSubmit();

    await vi.waitFor(() => expect(q('[role="alert"]')?.textContent).toContain("No free line there right now"));
  });

  it("clears the city search on Escape, and only then leaves the form", async () => {
    signIn.dispose();
    await openHome();
    use.citiesAnswers = { DE: Array.from({ length: 10 }, (_, i) => ({ city: `City ${i}`, lines: 1 })) };
    await openUse();
    await startAdding();
    type("#city-query", "7");
    await vi.waitFor(() => expect(qa('input[name="city"]').length).toBe(2));

    q("#city-query")!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));

    await vi.waitFor(() => expect(q<HTMLInputElement>("#city-query")?.value).toBe(""));
    expect(q('[data-form="add-device"]')).not.toBeNull();
    q("#city-query")!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await vi.waitFor(() => expect(document.activeElement).toBe(q('[data-action="use-add"]')));
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

  it("keeps a typed name while the city changes", async () => {
    await startAdding();
    type("#device-name", "Travel router");

    pick("Berlin");

    expect(q<HTMLInputElement>("#device-name")?.value).toBe("Travel router");
  });

  it("says where each device's line is", () => {
    const row = q(`.device[data-peer="${laptop.peerId}"]`)!;
    expect([...row.querySelectorAll("p")].map((p) => p.textContent)).toEqual(["Berlin, Germany", "Deutsche Telekom line"]);
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

  const choose = (name: string, value: string) => {
    const radio = q<HTMLInputElement>(`input[name="${name}"][value="${value}"]`)!;
    radio.click();
    radio.dispatchEvent(new Event("change", { bubbles: true }));
  };

  it("keeps its line by default, and says where that is", async () => {
    await openChange();

    expect(q<HTMLInputElement>('input[name="move"][value="keep"]')?.checked).toBe(true);
    expect(q(".move .failover-option")?.textContent?.replace(/\s+/g, " ").trim()).toBe(
      `Keep its line${laptop.city}, ${countryName("DE")}. Deutsche Telekom line`,
    );
    expect(q("#device-country")).toBeNull();
  });

  it("walks focus through a move, and back to the device", async () => {
    q(`[data-action="use-edit"][data-peer="${laptop.peerId}"]`)!.focus();
    await openChange();

    choose("move", "move");
    await vi.waitFor(() => expect(q('input[name="city"][value="Munich"]')).not.toBeNull());
    choose("city", "Munich");
    choose("failover", "off");
    q<HTMLFormElement>('[data-form="edit-device"]')!.requestSubmit();

    await vi.waitFor(() => expect(announcer.textContent).toBe("Changes saved."));
    expect(use.updates).toEqual([[laptop.peerId, { country: "DE", city: "Munich", failover: "off" }]]);
    expect(document.activeElement).toBe(q(`[data-action="use-edit"][data-peer="${laptop.peerId}"]`));
    expect([...q(`.device[data-peer="${laptop.peerId}"]`)!.querySelectorAll("p")].map((p) => p.textContent)).toEqual([
      `Munich, ${countryName("DE")}`,
      "M-net line",
    ]);
  });

  it("names the backup choices after the region it moves to", async () => {
    await openChange();
    const choices = () => [...app.querySelectorAll(".failover:not(.move) .failover-option")].map((o) => o.textContent?.replace(/\s+/g, " ").trim());
    expect(choices()[0]).toBe(`Use another line in Berlin, or else in ${countryName("DE")}`);
    expect(choices()[1]).toBe("Use another line in Berlin only");

    choose("move", "move");
    await vi.waitFor(() => expect(q('input[name="city"][value="Munich"]')).not.toBeNull());
    choose("city", "Munich");

    await vi.waitFor(() => expect(choices()[1]).toBe("Use another line in Munich only"));
  });

  it("leaves on Escape, with focus back on the device", async () => {
    await openChange();

    q('input[name="failover"]')!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));

    await vi.waitFor(() => expect(document.activeElement).toBe(q(`[data-action="use-edit"][data-peer="${laptop.peerId}"]`)));
    expect(use.updates).toEqual([]);
  });

  it("keeps focus on a failover the member picks with the keyboard, also when an error goes away", async () => {
    await openChange();
    use.updateAnswer = serviceError("no_free_line");
    choose("move", "move");
    await vi.waitFor(() => expect(q('input[name="city"][value="Munich"]')).not.toBeNull());
    q<HTMLFormElement>('[data-form="edit-device"]')!.requestSubmit();
    await vi.waitFor(() => expect(q('.use-step [role="alert"]')).not.toBeNull());

    q<HTMLInputElement>('input[name="failover"][value="off"]')!.focus();
    choose("failover", "off");

    await vi.waitFor(() => expect(q('.use-step [role="alert"]')).toBeNull());
    expect(document.activeElement).toBe(q('input[name="failover"][value="off"]'));
  });

  it("says when the region has no other free line", async () => {
    await openChange();
    use.updateAnswer = serviceError("no_free_line");
    choose("move", "move");
    await vi.waitFor(() => expect(q('input[name="city"][value="Munich"]')).not.toBeNull());

    q<HTMLFormElement>('[data-form="edit-device"]')!.requestSubmit();

    await vi.waitFor(() => expect(q('.use-step [role="alert"]')?.textContent).toContain("No free line there right now"));
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
    use.countriesAnswer = [];
    await openUse();

    q('[data-action="use-add"]')!.click();

    await vi.waitFor(() => expect(q(".use-step .empty")?.textContent).toContain("Nobody shares a line"));
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
