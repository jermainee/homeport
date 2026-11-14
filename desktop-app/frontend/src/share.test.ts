import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { Share } from "./share";
import { serviceError } from "./test/fake-auth";
import { FakeShare, off } from "./test/fake-share";

let api: FakeShare;
let share: Share;

beforeEach(() => {
  api = new FakeShare();
});

afterEach(() => {
  share?.dispose();
});

async function open(identity = "verified") {
  share = new Share(api, identity);
  await share.start();
}

describe("a member who isn't verified", () => {
  it("is asked to verify first, and Share stays untouched", async () => {
    await open("unverified");

    expect(share.view).toEqual({ kind: "verify", identity: "unverified", checking: false });
    expect(api.toggles).toEqual([]);
  });

  it("gets the switch once a new check finds them verified", async () => {
    await open("processing");
    api.identityStatus = "verified";

    const seen: string[] = [];
    share.subscribe((v) => seen.push(v.kind === "verify" && v.checking ? "checking" : v.kind));
    await share.recheck();

    expect(seen).toEqual(["checking", "ready"]);
    expect(share.view).toEqual({ kind: "ready", status: off, turning: false });
  });

  it("stays on the verify screen while the check is still running", async () => {
    await open("processing");
    api.identityStatus = "processing";

    await share.recheck();

    expect(share.view).toEqual({ kind: "verify", identity: "processing", checking: false });
    expect(api.identityChecks).toBe(1);
  });

  it("gets the switch after verifying, although Go refused a press before", async () => {
    await open();
    api.answer = { ...off, state: "error", error: "identity_required" };
    await share.toggle();
    expect(share.view).toMatchObject({ kind: "verify" });

    // Go still remembers the refusal; the API now says verified.
    api.identityStatus = "verified";
    await share.recheck();

    expect(share.view).toEqual({ kind: "ready", status: off, turning: false });
  });

  it("follows Share turned on from the tray once the API accepts the member", async () => {
    await open("processing");

    api.emit({ state: "connecting", sharing: true });

    expect(share.view).toMatchObject({ kind: "ready", status: { state: "connecting", sharing: true } });
  });

  it("goes to sign-in when Check again finds nobody signed in", async () => {
    await open("unverified");
    api.identityStatus = null;

    await share.recheck();

    expect(share.view).toEqual({ kind: "signedOut" });
  });

  it("is sent to verify when Go refuses to turn Share on", async () => {
    await open();
    api.answer = { ...off, state: "error", error: "identity_required" };

    await share.toggle();

    expect(share.view).toEqual({ kind: "verify", identity: "unverified", checking: false });
  });
});

describe("the Share switch", () => {
  it("shows Share as Go has it", async () => {
    api.current = { state: "online", sharing: true, bytesToday: 1_250_000, flowsActive: 2 };

    await open();

    expect(share.view).toEqual({ kind: "ready", status: api.current, turning: false });
  });

  it("turns Share on and follows what Go publishes", async () => {
    await open();
    const seen: string[] = [];
    share.subscribe((v) => v.kind === "ready" && seen.push(v.turning ? "turning" : v.status.state));

    await share.toggle();
    api.emit({ state: "online", flowsActive: 1 });
    api.emit({ state: "paused" });

    expect(api.toggles).toEqual([true]);
    expect(seen).toEqual(["turning", "connecting", "online", "paused"]);
  });

  it("turns Share off", async () => {
    api.current = { state: "online", sharing: true, bytesToday: 0, flowsActive: 0 };
    await open();

    await share.toggle();

    expect(api.toggles).toEqual([false]);
    expect(share.view).toMatchObject({ kind: "ready", status: { state: "off", sharing: false } });
  });

  it("ignores a second press while the first is on its way", async () => {
    await open();
    api.hold();

    const first = share.toggle();
    await share.toggle();
    api.release();
    await first;

    expect(api.toggles).toEqual([true]);
  });

  it("keeps the switch where it was when Go can't be reached, and says so", async () => {
    await open();
    api.answer = serviceError("unexpected");

    await share.toggle();

    expect(share.view).toEqual({ kind: "ready", status: off, turning: false, failed: "unexpected" });
  });

  it("forgets the failure on the next press", async () => {
    await open();
    api.answer = serviceError("unexpected");
    await share.toggle();

    await share.toggle();

    expect(share.view).toMatchObject({ kind: "ready", status: { state: "connecting" }, turning: false });
    expect(share.view).not.toHaveProperty("failed");
  });

  it("notices when the session is gone", async () => {
    await open();

    api.emit({ state: "error", sharing: false, error: "signed_out" });

    expect(share.view).toEqual({ kind: "signedOut" });
  });

  // Share starts right after the window found the member signed in, so a
  // signed_out Go still holds is from before. Going back to sign-in for it
  // would find the member signed in, start Share, and read it again.
  it("shows the switch off when Go still holds a signed_out from before the sign-in", async () => {
    api.current = { ...off, state: "error", error: "signed_out" };

    await open();

    expect(share.view).toEqual({ kind: "ready", status: off, turning: false });
  });
});

describe("after dispose", () => {
  it("stops listening to Go", async () => {
    await open();
    expect(api.listening).toBe(1);

    share.dispose();

    expect(api.listening).toBe(0);
  });
});
