import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { backupLine, CHECKOUT_GIVE_UP_MS, CHECKOUT_POLL_MS, httpsOnlyOf, Use, type UseView } from "./use";
import { serviceError } from "./test/fake-auth";
import { FakeUse, balance, berlin, france, germany, laptop, lyon, munich, newDevice, portugal } from "./test/fake-use";

let api: FakeUse;
let use: Use;

beforeEach(() => {
  vi.useFakeTimers();
  api = new FakeUse();
});

afterEach(() => {
  use?.dispose();
  vi.useRealTimers();
});

async function open(identity = "verified") {
  use = new Use(api, identity);
  await use.start();
}

/** ready returns the view, failing the test unless Use is ready. */
function ready(): Extract<UseView, { kind: "ready" }> {
  const v = use.view;
  if (v.kind !== "ready") throw new Error(`view is ${v.kind}, want ready`);
  return v;
}

const bought = { purchased: 1_025_000, earned: 5_000, total: 1_030_000, bytes: 103_000_000_000 };

describe("opening Use", () => {
  it("shows the balance and the member's devices, and loads no lines", async () => {
    await open();

    expect(api.calls.sort()).toEqual(["balance", "devices"]);
    expect(use.view).toEqual({
      kind: "ready",
      balance,
      devices: [laptop],
      checkout: { phase: "idle" },
      step: { kind: "list" },
    });
  });

  it("asks an unverified member to verify first, and loads nothing", async () => {
    await open("requires_input");

    expect(use.view).toEqual({ kind: "verify" });
    expect(api.calls).toEqual([]);
  });

  it("loads once the member is verified", async () => {
    await open("processing");

    await use.unlock();

    expect(ready().balance).toEqual(balance);
  });

  it("offers a retry when Homeport can't be reached", async () => {
    api.devicesAnswer = serviceError("offline");
    await open();
    expect(use.view).toEqual({ kind: "failed", error: "offline" });

    api.devicesAnswer = [laptop];
    await use.retry();

    expect(ready().devices).toEqual([laptop]);
  });

  it("goes to sign-in when the session ended", async () => {
    api.balanceAnswer = serviceError("signed_out");

    await open();

    expect(use.view).toEqual({ kind: "signedOut" });
  });

  it("asks to verify when the API says the member isn't verified", async () => {
    api.devicesAnswer = serviceError("identity_required");

    await open();

    expect(use.view).toEqual({ kind: "verify" });
  });

  it("keeps a removal that finished while a refresh was on its way", async () => {
    await open();
    let answer!: (d: typeof laptop[]) => void;
    api.devices = () => new Promise((r) => (answer = r));
    const refreshing = use.refresh();

    use.askRemove(laptop.peerId);
    await use.confirmRemove();
    answer([laptop]); // the list from before the removal
    await refreshing;

    expect(ready().devices).toEqual([]);
  });

  it("keeps credits that arrived while a refresh was on its way", async () => {
    await open();
    let answer!: (b: typeof balance) => void;
    api.balance = () => new Promise((r) => (answer = r));
    const refreshing = use.refresh();
    api.checkoutPolls = [{ arrived: true, balance: bought }];
    await use.buy();
    await vi.advanceTimersByTimeAsync(CHECKOUT_POLL_MS);

    answer(balance); // the balance from before the purchase
    await refreshing;

    expect(ready().balance).toEqual(bought);
  });

  it("follows the devices alone, to show where each one exits", async () => {
    await open();
    const backup = { ...laptop, exitLine: { lineId: "line-porto", country: "PT", city: "", since: "2026-11-14T13:02:10Z" } };
    api.devicesAnswer = [backup];
    const calls = api.calls.length;

    await use.refreshDevices();

    expect(ready().devices).toEqual([backup]);
    expect(api.calls.slice(calls)).toEqual(["devices"]);
  });

  it("keeps the devices on screen when following them fails for a moment", async () => {
    await open();
    api.devicesAnswer = serviceError("offline");

    await use.refreshDevices();

    expect(ready().devices).toEqual([laptop]);
  });

  it("goes to sign-in when following the devices finds the session ended", async () => {
    await open();
    api.devicesAnswer = serviceError("signed_out");

    await use.refreshDevices();

    expect(use.view).toEqual({ kind: "signedOut" });
  });

  it("keeps a change that finished while the devices were followed", async () => {
    await open();
    let answer!: (d: typeof laptop[]) => void;
    api.devices = () => new Promise((r) => (answer = r));
    const following = use.refreshDevices();

    use.startEditing(laptop.peerId);
    use.pickFailover("off");
    api.devices = FakeUse.prototype.devices;
    await use.saveDevice();
    answer([laptop]); // the list from before the change
    await following;

    expect(ready().devices).toEqual([{ ...laptop, failover: "off" }]);
  });

  it("refreshes in place, keeping what is on screen", async () => {
    await open();
    api.balanceAnswer = bought;
    const seen: string[] = [];
    use.subscribe((v) => seen.push(v.kind));

    await use.refresh();

    expect(seen).toEqual(["ready"]);
    expect(ready().balance).toEqual(bought);
  });
});

describe("buying credits", () => {
  it("waits in the app while the member pays, until the credits arrive", async () => {
    await open();
    api.checkoutPolls = [{ arrived: false, balance }, { arrived: true, balance: bought }];

    await use.buy();
    expect(ready().checkout).toEqual({ phase: "waiting" });
    await vi.advanceTimersByTimeAsync(CHECKOUT_POLL_MS);
    expect(ready().checkout).toEqual({ phase: "waiting" });
    await vi.advanceTimersByTimeAsync(CHECKOUT_POLL_MS);

    expect(ready().checkout).toEqual({ phase: "idle", arrived: true });
    expect(ready().balance).toEqual(bought);
    await vi.advanceTimersByTimeAsync(CHECKOUT_POLL_MS * 3);
    expect(api.count("pollCheckout")).toBe(2);
  });

  it("opens a new Checkout while waiting, for a member who left the first", async () => {
    await open();
    await use.buy();
    expect(ready().checkout).toEqual({ phase: "waiting" });

    await use.buy();

    expect(api.count("buyCredits")).toBe(2);
    expect(ready().checkout).toEqual({ phase: "waiting" });
  });

  it("opens a new Checkout after waiting gave up", async () => {
    await open();
    await use.buy();
    await vi.advanceTimersByTimeAsync(CHECKOUT_GIVE_UP_MS + CHECKOUT_POLL_MS);

    await use.buy();

    expect(api.count("buyCredits")).toBe(2);
    expect(ready().checkout).toEqual({ phase: "waiting" });
  });

  it("is busy while Checkout opens, and ignores a second press", async () => {
    await open();
    api.hold();
    const pending = use.buy();

    expect(ready().checkout).toEqual({ phase: "opening" });
    await use.buy();
    api.release();
    await pending;

    expect(api.count("buyCredits")).toBe(1);
  });

  it("says why Checkout didn't open", async () => {
    await open();
    api.buyAnswer = serviceError("rate_limited");

    await use.buy();

    expect(ready().checkout).toEqual({ phase: "idle", error: "rate_limited" });
  });

  it("keeps waiting while Homeport can't be reached, and says so", async () => {
    await open();
    api.checkoutPolls = [serviceError("offline"), { arrived: true, balance: bought }];
    await use.buy();

    await vi.advanceTimersByTimeAsync(CHECKOUT_POLL_MS);
    expect(ready().checkout).toEqual({ phase: "waiting", offline: true });
    await vi.advanceTimersByTimeAsync(CHECKOUT_POLL_MS * 2);

    expect(ready().checkout).toEqual({ phase: "idle", arrived: true });
  });

  it("stops waiting after a while, and Check again picks it up", async () => {
    await open();
    await use.buy();

    await vi.advanceTimersByTimeAsync(CHECKOUT_GIVE_UP_MS + CHECKOUT_POLL_MS);
    expect(ready().checkout).toEqual({ phase: "stalled" });

    api.checkoutPolls = [{ arrived: true, balance: bought }];
    await use.checkPayment();
    expect(ready().checkout).toEqual({ phase: "idle", arrived: true });
  });

  it("shows the balance as it moves while waiting", async () => {
    await open();
    const spent = { ...balance, purchased: 24_000, total: 29_000, bytes: 2_900_000_000 };
    api.checkoutPolls = [{ arrived: false, balance: spent }];
    await use.buy();

    await vi.advanceTimersByTimeAsync(CHECKOUT_POLL_MS);

    expect(ready().balance).toEqual(spent);
  });
});

describe("adding a device", () => {
  /** The add step, failing the test unless the member is adding a device. */
  function adding() {
    const step = ready().step;
    if (step.kind !== "add") throw new Error(`step is ${step.kind}, want add`);
    return step;
  }

  it("loads the countries, and picks the one the newest device is in, with its cities", async () => {
    await open();

    await use.startAdding();

    expect(adding()).toEqual({
      kind: "add",
      countries: [germany, france],
      country: "DE",
      cities: [berlin, munich],
      city: "",
      query: "",
      name: "",
      creating: false,
    });
  });

  it("shows the form at once, and the countries once they arrive", async () => {
    await open();
    api.holdRegions();

    const loading = use.startAdding();
    const first = adding();
    api.releaseRegions();
    await loading;

    expect(first).toMatchObject({ countries: null, country: null, cities: null });
    expect(adding().countries).toEqual([germany, france]);
  });

  it("picks no country for a member without devices, when there are several", async () => {
    api.devicesAnswer = [];
    await open();

    await use.startAdding();

    expect(adding()).toMatchObject({ country: null, cities: null });
    expect(api.calls.some((c) => c.startsWith("cities"))).toBe(false);
  });

  it("picks the only country there is", async () => {
    api.devicesAnswer = [];
    api.countriesAnswer = [portugal];
    api.citiesAnswers = { PT: [] };
    await open();

    await use.startAdding();

    expect(adding()).toMatchObject({ country: "PT", cities: [] });
  });

  it("shows the cities of the country the member picks, starting from any city", async () => {
    await open();
    await use.startAdding();
    use.pickCity("Munich");
    use.setQuery("mu");

    await use.pickCountry("FR");

    expect(adding()).toMatchObject({ country: "FR", cities: [lyon], city: "", query: "" });
  });

  it("keeps the cities of the country picked last when answers cross", async () => {
    await open();
    await use.startAdding();
    let answerFrance!: (c: typeof lyon[]) => void;
    api.cities = (country: string) =>
      country === "FR" ? new Promise((r) => (answerFrance = r)) : Promise.resolve([berlin, munich]);

    const france = use.pickCountry("FR");
    await use.pickCountry("DE");
    answerFrance([lyon]);
    await france;

    expect(adding()).toMatchObject({ country: "DE", cities: [berlin, munich] });
  });

  it("creates the config in the city the member picked and shows it", async () => {
    await open();
    await use.startAdding();
    use.pickCity("Munich");
    use.setName("  iPhone ");

    await use.create();

    const created = newDevice("DE", "Munich", "iPhone");
    expect(api.calls).toContain("addDevice DE Munich iPhone");
    expect(ready().step).toEqual({ kind: "config", device: created, saving: false, saved: false });
    expect(ready().devices).toEqual([created.device, laptop]);
  });

  it("creates the config in any city of the country when no city is picked", async () => {
    await open();
    await use.startAdding();
    use.setName("Laptop");

    await use.create();

    expect(api.calls).toContain("addDevice DE - Laptop");
  });

  it("is busy while the config is created", async () => {
    await open();
    await use.startAdding();
    use.setName("iPhone");
    api.hold();

    const pending = use.create();
    expect(adding()).toMatchObject({ creating: true });
    await use.create();
    api.release();
    await pending;

    expect(api.calls.filter((c) => c.startsWith("addDevice"))).toHaveLength(1);
  });

  it("needs a name before it asks the API", async () => {
    await open();
    await use.startAdding();
    use.setName("   ");

    await use.create();

    expect(adding()).toMatchObject({ name: "   ", creating: false, error: "invalid_name" });
    expect(api.calls.some((c) => c.startsWith("addDevice"))).toBe(false);
  });

  it("needs a country", async () => {
    api.devicesAnswer = [];
    await open();
    await use.startAdding();
    use.setName("iPhone");

    await use.create();

    expect(adding()).toMatchObject({ country: null, error: "no_country" });
    expect(api.calls.some((c) => c.startsWith("addDevice"))).toBe(false);
  });

  it("reloads the region when it has no free line any more, and goes back to any city once the city is gone", async () => {
    await open();
    await use.startAdding();
    use.pickCity("Munich");
    use.setName("iPhone");
    api.addAnswer = serviceError("no_free_line");
    api.countriesAnswer = [{ country: "DE", lines: 2 }, france];
    api.citiesAnswers = { DE: [berlin] };

    await use.create();

    expect(adding()).toMatchObject({
      countries: [{ country: "DE", lines: 2 }, france],
      country: "DE",
      cities: [berlin],
      city: "",
      name: "iPhone",
      creating: false,
      error: "no_free_line",
    });
  });

  it("forgets the country too when no line is left in it", async () => {
    await open();
    await use.startAdding();
    use.setName("iPhone");
    api.addAnswer = serviceError("no_free_line");
    api.countriesAnswer = [france];

    await use.create();

    expect(adding()).toMatchObject({ countries: [france], country: null, cities: null, error: "no_free_line" });
  });

  it("keeps the country the member picks while the countries reload", async () => {
    await open();
    await use.startAdding();
    use.setName("iPhone");
    api.addAnswer = serviceError("no_free_line");
    api.holdRegions();

    const failing = use.create();
    await vi.waitFor(() => expect(api.count("countries")).toBe(2));
    const picking = use.pickCountry("FR");
    api.releaseRegions();
    await failing;
    await picking;

    expect(adding()).toMatchObject({ country: "FR", cities: [lyon], error: "no_free_line" });
  });

  it("says why a country's cities didn't load, and loads them again on request", async () => {
    api.citiesAnswers = { DE: serviceError("offline") };
    await open();
    await use.startAdding();
    expect(adding()).toMatchObject({ country: "DE", cities: [], citiesError: "offline" });

    api.citiesAnswers = { DE: [berlin] };
    await use.reloadCities();

    expect(adding()).toMatchObject({ country: "DE", cities: [berlin] });
    expect(adding().citiesError).toBeUndefined();
  });

  it("picks the newest device's country again when the countries load on a second try", async () => {
    api.countriesAnswer = serviceError("offline");
    await open();
    await use.startAdding();

    api.countriesAnswer = [germany, france];
    await use.reloadRegions();

    expect(adding()).toMatchObject({ country: "DE", cities: [berlin, munich] });
  });

  it("says why the countries didn't load, and loads them again on request", async () => {
    await open();
    api.countriesAnswer = serviceError("offline");
    await use.startAdding();
    expect(adding()).toMatchObject({ countries: [], error: "offline" });

    api.countriesAnswer = [germany];
    await use.reloadRegions();

    expect(adding()).toMatchObject({ countries: [germany], country: "DE", cities: [berlin, munich] });
    expect(adding().error).toBeUndefined();
  });

  it("goes to sign-in when the session ended while the countries load", async () => {
    await open();
    api.countriesAnswer = serviceError("signed_out");

    await use.startAdding();

    expect(use.view).toEqual({ kind: "signedOut" });
  });

  it("goes back to the list on Cancel, and stays there when the countries arrive late", async () => {
    await open();
    api.holdRegions();
    const loading = use.startAdding();

    use.cancelAdding();
    api.releaseRegions();
    await loading;

    expect(ready().step).toEqual({ kind: "list" });
  });
});

describe("the new device's config", () => {
  async function created() {
    await open();
    await use.startAdding();
    use.setName("iPhone");
    await use.create();
    return newDevice("DE", "", "iPhone");
  }

  it("is saved where the member chooses", async () => {
    const dev = await created();

    await use.save();

    expect(api.calls).toContain(`saveConfig ${dev.device.peerId}`);
    expect(ready().step).toMatchObject({ kind: "config", saving: false, saved: true });
  });

  it("isn't marked saved when the member cancels the dialog", async () => {
    await created();
    api.saveAnswer = false;

    await use.save();

    expect(ready().step).toMatchObject({ kind: "config", saving: false, saved: false });
  });

  it("says why it couldn't be saved", async () => {
    await created();
    api.saveAnswer = serviceError("unexpected");

    await use.save();

    expect(ready().step).toMatchObject({ kind: "config", saving: false, saved: false, error: "unexpected" });
  });

  it("is forgotten for good on Done", async () => {
    const dev = await created();

    await use.done();

    expect(api.forgotten).toEqual([dev.device.peerId]);
    expect(ready().step).toEqual({ kind: "list" });
  });

  it("is forgotten when Use closes, so the key doesn't outlive the screen", async () => {
    const dev = await created();

    use.dispose();

    expect(api.forgotten).toEqual([dev.device.peerId]);
  });
});

describe("changing a device", () => {
  const exitOn = (lineId: string) => ({ lineId, country: "DE", city: "Berlin", since: "2026-11-14T13:02:10Z" });
  const movedLine = "line-DE-Munich-2";

  /** The edit step, failing the test unless the member is changing a device. */
  function editing() {
    const step = ready().step;
    if (step.kind !== "edit") throw new Error(`step is ${step.kind}, want edit`);
    return step;
  }

  /** Opens the laptop's form and chooses to move it, to Munich unless told otherwise. */
  async function moving(city = "Munich") {
    use.startEditing(laptop.peerId);
    await use.setMove(true);
    use.pickCity(city);
  }

  it("does not call the old line a backup line while the device moves to its new one", async () => {
    await open();
    await moving();
    // The gateway moves the device at its next config fetch; until then the
    // API reports the session on the old line.
    api.updateAnswer = { ...laptop, lineId: movedLine, exitLine: exitOn(laptop.lineId) };

    await use.saveDevice();
    const saved = backupLine(ready().devices[0]!);
    api.devicesAnswer = [{ ...laptop, lineId: movedLine, exitLine: exitOn(laptop.lineId) }];
    await use.refreshDevices();
    const refreshed = backupLine(ready().devices[0]!);

    expect(saved).toBeUndefined();
    expect(refreshed).toBeUndefined();
  });

  it("shows a backup line again once the device has left its old line", async () => {
    await open();
    await moving();
    api.updateAnswer = { ...laptop, lineId: movedLine, exitLine: exitOn(laptop.lineId) };
    await use.saveDevice();

    api.devicesAnswer = [{ ...laptop, lineId: movedLine, exitLine: null }];
    await use.refreshDevices();
    api.devicesAnswer = [{ ...laptop, lineId: movedLine, exitLine: exitOn(laptop.lineId) }];
    await use.refreshDevices();

    expect(backupLine(ready().devices[0]!)?.lineId).toBe(laptop.lineId);
  });

  it("says so when the device was removed while its form was open", async () => {
    await open();
    use.startEditing(laptop.peerId);
    use.pickFailover("off");
    api.devicesAnswer = [];
    await use.refresh();

    await use.saveDevice();

    expect(editing()).toMatchObject({ peerId: laptop.peerId, error: "device_gone" });
    expect(api.updates).toEqual([]);
  });

  it("starts from the device's failover, keeping its line, with a move's region at its city", async () => {
    api.devicesAnswer = [{ ...laptop, failover: "city" }];
    await open();

    use.startEditing(laptop.peerId);

    expect(editing()).toEqual({
      kind: "edit",
      peerId: laptop.peerId,
      move: false,
      countries: null,
      country: "DE",
      cities: null,
      city: "Berlin",
      query: "",
      failover: "city",
      saving: false,
    });
    expect(api.calls).not.toContain("countries");
  });

  it("loads where lines are free once the member chooses to move it, keeping its city", async () => {
    await open();
    use.startEditing(laptop.peerId);

    await use.setMove(true);

    expect(editing()).toMatchObject({ move: true, countries: [germany, france], country: "DE", cities: [berlin, munich], city: "Berlin" });
  });

  it("counts where lines are free without the device's own line", async () => {
    await open();

    await moving();

    expect(api.calls).toContain(`countries ${laptop.peerId}`);
    expect(api.calls).toContain(`cities DE ${laptop.peerId}`);
  });

  it("moves the device to another free line in the region, and keeps its failover", async () => {
    await open();
    await moving();

    await use.saveDevice();

    expect(api.updates).toEqual([[laptop.peerId, { country: "DE", city: "Munich" }]]);
    expect(ready().step).toEqual({ kind: "list" });
    expect(ready().devices).toEqual([{ ...laptop, lineId: movedLine, city: "Munich", isp: "M-net" }]);
  });

  it("moves the device anywhere in another country", async () => {
    await open();
    use.startEditing(laptop.peerId);
    await use.setMove(true);
    await use.pickCountry("FR");

    await use.saveDevice();

    expect(api.updates).toEqual([[laptop.peerId, { country: "FR", city: "" }]]);
  });

  it("needs a country to move to", async () => {
    api.devicesAnswer = [{ ...laptop, country: "", city: "" }];
    await open();
    use.startEditing(laptop.peerId);
    await use.setMove(true);

    await use.saveDevice();

    expect(editing()).toMatchObject({ country: null, error: "no_country" });
    expect(api.updates).toEqual([]);
  });

  it("changes only the failover when it keeps its line", async () => {
    await open();
    use.startEditing(laptop.peerId);
    use.pickFailover("off");

    await use.saveDevice();

    expect(api.updates).toEqual([[laptop.peerId, { failover: "off" }]]);
    expect(ready().devices).toEqual([{ ...laptop, failover: "off" }]);
  });

  it("goes back without asking Homeport when nothing changed", async () => {
    await open();
    await moving();
    await use.setMove(false);

    await use.saveDevice();

    expect(api.updates).toEqual([]);
    expect(ready().step).toEqual({ kind: "list" });
  });

  it("is busy while it saves, and ignores a second press, Cancel and a new pick", async () => {
    await open();
    await moving();
    api.hold();

    const pending = use.saveDevice();
    expect(editing()).toMatchObject({ saving: true });
    await use.saveDevice();
    use.cancelEditing();
    use.pickCity("Berlin");
    await use.setMove(false);
    expect(editing()).toMatchObject({ move: true, city: "Munich", saving: true });
    api.release();
    await pending;

    expect(api.updates).toEqual([[laptop.peerId, { country: "DE", city: "Munich" }]]);
    expect(ready().step).toEqual({ kind: "list" });
  });

  it.each(["offline", "rate_limited", "unexpected"] as const)("keeps the form and says why on %s", async (code) => {
    await open();
    await moving();
    api.updateAnswer = serviceError(code);

    await use.saveDevice();

    expect(editing()).toMatchObject({ move: true, country: "DE", city: "Munich", saving: false, error: code });
    expect(ready().devices).toEqual([laptop]);
  });

  it("reloads where lines are free when the region has no other free line, and goes back to any city once the city is gone", async () => {
    await open();
    await moving();
    api.updateAnswer = serviceError("no_free_line");
    api.citiesAnswers = { DE: [berlin] };

    await use.saveDevice();

    expect(editing()).toMatchObject({ country: "DE", cities: [berlin], city: "", saving: false, error: "no_free_line" });
  });

  it("drops a device that was removed meanwhile, and goes back to the list", async () => {
    await open();
    use.startEditing(laptop.peerId);
    use.pickFailover("off");
    api.updateAnswer = serviceError("device_gone");

    await use.saveDevice();

    expect(ready().devices).toEqual([]);
    expect(ready().step).toEqual({ kind: "list" });
  });

  it("goes to sign-in when the session ended", async () => {
    await open();
    use.startEditing(laptop.peerId);
    use.pickFailover("off");
    api.updateAnswer = serviceError("signed_out");

    await use.saveDevice();

    expect(use.view).toEqual({ kind: "signedOut" });
  });

  it("goes back to the list on Cancel", async () => {
    await open();
    await moving();

    use.cancelEditing();

    expect(ready().step).toEqual({ kind: "list" });
    expect(api.updates).toEqual([]);
  });

  it("opens only from the list, for a device the member has", async () => {
    await open();

    use.startEditing("peer-unknown");
    expect(ready().step).toEqual({ kind: "list" });

    await use.startAdding();
    use.startEditing(laptop.peerId);
    expect(ready().step).toMatchObject({ kind: "add" });
  });

  it("keeps a change that finished while a refresh was on its way", async () => {
    await open();
    let answer!: (d: typeof laptop[]) => void;
    api.devices = () => new Promise((r) => (answer = r));
    const refreshing = use.refresh();

    await moving();
    await use.saveDevice();
    answer([laptop]); // the list from before the change
    await refreshing;

    expect(ready().devices).toEqual([{ ...laptop, lineId: movedLine, city: "Munich", isp: "M-net" }]);
  });
});

describe("removing a device", () => {
  it("asks first, then removes it", async () => {
    await open();

    use.askRemove(laptop.peerId);
    expect(ready().removing).toEqual({ peerId: laptop.peerId, busy: false });
    await use.confirmRemove();

    expect(api.removed).toEqual([laptop.peerId]);
    expect(ready().devices).toEqual([]);
    expect(ready().removing).toBeUndefined();
  });

  it("keeps the device when the member changes their mind", async () => {
    await open();

    use.askRemove(laptop.peerId);
    use.cancelRemove();

    expect(ready().removing).toBeUndefined();
    expect(ready().devices).toEqual([laptop]);
  });

  it("says why the device wasn't removed", async () => {
    await open();
    api.removeAnswer = serviceError("offline");

    use.askRemove(laptop.peerId);
    await use.confirmRemove();

    expect(ready().removing).toEqual({ peerId: laptop.peerId, busy: false, error: "offline" });
    expect(ready().devices).toEqual([laptop]);
  });
});

describe("HTTPS only", () => {
  it("turns off for one device, and on again", async () => {
    await open();
    expect(httpsOnlyOf(ready(), laptop)).toEqual({ on: true, busy: false });

    await use.toggleHttpsOnly(laptop.peerId);
    expect(api.httpsOnly).toEqual([[laptop.peerId, false]]);
    expect(ready().devices).toEqual([{ ...laptop, httpsOnly: false }]);
    expect(httpsOnlyOf(ready(), ready().devices[0]!)).toEqual({ on: false, busy: false });

    await use.toggleHttpsOnly(laptop.peerId);
    expect(api.httpsOnly).toEqual([
      [laptop.peerId, false],
      [laptop.peerId, true],
    ]);
    expect(ready().devices).toEqual([laptop]);
  });

  it("shows the position asked for while it turns, and ignores a second press", async () => {
    await open();
    api.hold();

    const turning = use.toggleHttpsOnly(laptop.peerId);
    expect(httpsOnlyOf(ready(), laptop)).toEqual({ on: false, busy: true });
    void use.toggleHttpsOnly(laptop.peerId);
    api.release();
    await turning;

    expect(api.httpsOnly).toEqual([[laptop.peerId, false]]);
    expect(httpsOnlyOf(ready(), ready().devices[0]!)).toEqual({ on: false, busy: false });
  });

  it("says why it didn't turn, and goes back to where the device is", async () => {
    await open();
    api.httpsOnlyAnswer = serviceError("offline");

    await use.toggleHttpsOnly(laptop.peerId);

    expect(httpsOnlyOf(ready(), laptop)).toEqual({ on: true, busy: false, error: "offline" });
    expect(ready().devices).toEqual([laptop]);

    await use.toggleHttpsOnly(laptop.peerId);
    expect(httpsOnlyOf(ready(), ready().devices[0]!)).toEqual({ on: false, busy: false });
  });

  it("drops a device that was removed meanwhile", async () => {
    await open();
    api.httpsOnlyAnswer = serviceError("device_gone");

    await use.toggleHttpsOnly(laptop.peerId);

    expect(ready().devices).toEqual([]);
    expect(ready().httpsOnly?.[laptop.peerId]).toBeUndefined();
  });

  it("goes to sign-in when the session ended", async () => {
    await open();
    api.httpsOnlyAnswer = serviceError("signed_out");

    await use.toggleHttpsOnly(laptop.peerId);

    expect(use.view).toEqual({ kind: "signedOut" });
  });

  it("keeps a turn that finished while a refresh was on its way", async () => {
    await open();
    let answer!: (d: typeof laptop[]) => void;
    api.devices = () => new Promise((r) => (answer = r));
    const refreshing = use.refresh();

    await use.toggleHttpsOnly(laptop.peerId);
    answer([laptop]); // the list from before the turn
    await refreshing;

    expect(ready().devices).toEqual([{ ...laptop, httpsOnly: false }]);
  });

  it("turns two devices at once", async () => {
    const phone = { ...laptop, peerId: "peer-phone", name: "iPhone", httpsOnly: false };
    api.devicesAnswer = [laptop, phone];
    await open();
    api.hold();

    const first = use.toggleHttpsOnly(laptop.peerId);
    const second = use.toggleHttpsOnly(phone.peerId);
    expect(httpsOnlyOf(ready(), laptop)).toEqual({ on: false, busy: true });
    expect(httpsOnlyOf(ready(), phone)).toEqual({ on: true, busy: true });
    api.release();
    await Promise.all([first, second]);

    expect(ready().devices).toEqual([
      { ...laptop, httpsOnly: false },
      { ...phone, httpsOnly: true },
    ]);
    expect(ready().httpsOnly).toEqual({});
  });
});
