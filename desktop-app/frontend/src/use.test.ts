import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CHECKOUT_GIVE_UP_MS, CHECKOUT_POLL_MS, Use, type UseView } from "./use";
import { serviceError } from "./test/fake-auth";
import { FakeUse, balance, berlin, laptop, lyon, newDevice, porto } from "./test/fake-use";

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
  it("shows the balance, the lines and the member's devices", async () => {
    await open();

    expect(use.view).toEqual({
      kind: "ready",
      balance,
      lines: [berlin, lyon],
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
    api.linesAnswer = serviceError("offline");
    await open();
    expect(use.view).toEqual({ kind: "failed", error: "offline" });

    api.linesAnswer = [berlin];
    await use.retry();

    expect(ready().lines).toEqual([berlin]);
  });

  it("goes to sign-in when the session ended", async () => {
    api.balanceAnswer = serviceError("signed_out");

    await open();

    expect(use.view).toEqual({ kind: "signedOut" });
  });

  it("asks to verify when the API says the member isn't verified", async () => {
    api.linesAnswer = serviceError("identity_required");

    await open();

    expect(use.view).toEqual({ kind: "verify" });
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
  it("starts with the first online line picked", async () => {
    api.linesAnswer = [lyon, porto, berlin];
    await open();

    use.startAdding();

    expect(ready().step).toEqual({ kind: "add", lineId: porto.lineId, name: "", creating: false });
  });

  it("creates the config on the line the member picked and shows it", async () => {
    await open();
    use.startAdding();
    use.pickLine(lyon.lineId);
    use.setName("  iPhone ");

    await use.create();

    const created = newDevice(lyon.lineId, "iPhone");
    expect(api.calls).toContain(`addDevice ${lyon.lineId} iPhone`);
    expect(ready().step).toEqual({ kind: "config", device: created, saving: false, saved: false });
    expect(ready().devices).toEqual([created.device, laptop]);
  });

  it("is busy while the config is created", async () => {
    await open();
    use.startAdding();
    use.setName("iPhone");
    api.hold();

    const pending = use.create();
    expect(ready().step).toMatchObject({ kind: "add", creating: true });
    await use.create();
    api.release();
    await pending;

    expect(api.calls.filter((c) => c.startsWith("addDevice"))).toHaveLength(1);
  });

  it("needs a name before it asks the API", async () => {
    await open();
    use.startAdding();
    use.setName("   ");

    await use.create();

    expect(ready().step).toEqual({ kind: "add", lineId: berlin.lineId, name: "   ", creating: false, error: "invalid_name" });
    expect(api.calls.some((c) => c.startsWith("addDevice"))).toBe(false);
  });

  it("needs a line", async () => {
    api.linesAnswer = [];
    await open();
    use.startAdding();
    use.setName("iPhone");

    await use.create();

    expect(ready().step).toMatchObject({ kind: "add", lineId: null, error: "line_unavailable" });
    expect(api.calls.some((c) => c.startsWith("addDevice"))).toBe(false);
  });

  it("reloads the lines when the picked one is gone, and unpicks it", async () => {
    await open();
    use.startAdding();
    use.pickLine(lyon.lineId);
    use.setName("iPhone");
    api.addAnswer = serviceError("line_unavailable");
    api.linesAnswer = [berlin];

    await use.create();

    expect(ready().lines).toEqual([berlin]);
    expect(ready().step).toEqual({ kind: "add", lineId: null, name: "iPhone", creating: false, error: "line_unavailable" });
  });

  it("keeps the form when the line is full", async () => {
    await open();
    use.startAdding();
    use.setName("iPhone");
    api.addAnswer = serviceError("line_full");

    await use.create();

    expect(ready().step).toEqual({ kind: "add", lineId: berlin.lineId, name: "iPhone", creating: false, error: "line_full" });
  });

  it("goes back to the list on Cancel", async () => {
    await open();
    use.startAdding();

    use.cancelAdding();

    expect(ready().step).toEqual({ kind: "list" });
  });
});

describe("the new device's config", () => {
  async function created() {
    await open();
    use.startAdding();
    use.setName("iPhone");
    await use.create();
    return newDevice(berlin.lineId, "iPhone");
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
