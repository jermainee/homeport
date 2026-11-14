import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { IDENTITY_GIVE_UP_MS, IDENTITY_POLL_MS, Identity, type IdentityView } from "./identity";
import { serviceError } from "./test/fake-auth";
import { FakeIdentity } from "./test/fake-identity";

let api: FakeIdentity;
let identity: Identity;

beforeEach(() => {
  vi.useFakeTimers();
  api = new FakeIdentity();
});

afterEach(() => {
  identity?.dispose();
  vi.useRealTimers();
});

function open(status: string) {
  identity = new Identity(api, status);
  identity.start();
}

/** states records the identity state of every view change. */
function states(): string[] {
  const seen: string[] = [];
  identity.subscribe((v) => seen.push(v.kind === "status" ? v.state : v.kind));
  return seen;
}

const tick = () => vi.advanceTimersByTimeAsync(IDENTITY_POLL_MS);

describe("Verify identity", () => {
  it("opens the check and follows it until the member is verified", async () => {
    open("unverified");
    api.polls = ["waiting", "processing", "verified"];
    const seen = states();

    await identity.verify();
    expect(identity.view).toEqual({ kind: "status", state: "waiting" });
    await tick();
    await tick();
    await tick();

    expect(seen).toEqual(["unverified", "waiting", "processing", "verified"]);
    expect(identity.view).toEqual({ kind: "status", state: "verified" });
    // Verified is final: polling stops.
    await tick();
    expect(api.pollCount).toBe(3);
  });

  it("shows a failed check and stops", async () => {
    open("unverified");
    api.polls = ["processing", "requires_input"];

    await identity.verify();
    await tick();
    await tick();
    await tick();

    expect(identity.view).toEqual({ kind: "status", state: "requires_input" });
    expect(api.pollCount).toBe(2);
  });

  it("goes straight to verified when the API says so already", async () => {
    open("unverified");
    api.startAnswer = "verified";

    await identity.verify();
    await tick();

    expect(identity.view).toEqual({ kind: "status", state: "verified" });
    expect(api.pollCount).toBe(0);
  });

  it("is busy while the check is opened, and ignores a second press", async () => {
    open("requires_input");
    const pending = identity.verify();

    expect(identity.view).toEqual({ kind: "status", state: "requires_input", busy: "starting" });
    await identity.verify();
    await pending;

    expect(api.starts).toBe(1);
  });

  it("says why the check didn't open, and keeps the state", async () => {
    open("requires_input");
    api.startAnswer = serviceError("rate_limited");

    await identity.verify();
    await tick();

    expect(identity.view).toEqual({ kind: "status", state: "requires_input", error: "rate_limited" });
    expect(api.pollCount).toBe(0);
  });
});

describe("polling", () => {
  it("starts by itself when a check is being processed", async () => {
    open("processing");
    api.polls = ["processing", "verified"];

    await tick();
    await tick();

    expect(identity.view).toEqual({ kind: "status", state: "verified" });
  });

  it("doesn't poll when nothing is in progress", async () => {
    open("unverified");

    await tick();
    await tick();

    expect(api.pollCount).toBe(0);
  });

  it("keeps trying while Homeport can't be reached, and says so", async () => {
    open("unverified");
    await identity.verify();
    api.polls = [serviceError("offline"), "processing"];

    await tick();
    expect(identity.view).toEqual({ kind: "status", state: "waiting", offline: true });
    await vi.advanceTimersByTimeAsync(IDENTITY_POLL_MS * 2);

    expect(identity.view).toEqual({ kind: "status", state: "processing" });
  });

  it("stops waiting after a while, and Check again picks it up", async () => {
    open("unverified");
    await identity.verify();
    api.status = "waiting";

    await vi.advanceTimersByTimeAsync(IDENTITY_GIVE_UP_MS + IDENTITY_POLL_MS);
    const polls = api.pollCount;
    expect(identity.view).toEqual({ kind: "status", state: "waiting", stalled: true });
    await vi.advanceTimersByTimeAsync(IDENTITY_POLL_MS * 5);
    expect(api.pollCount).toBe(polls);

    api.status = "processing";
    await identity.check();
    expect(identity.view).toEqual({ kind: "status", state: "processing" });
    api.status = "verified";
    await tick();
    expect(identity.view).toEqual({ kind: "status", state: "verified" });
  });

  it("goes to sign-in when the session ended", async () => {
    open("unverified");
    await identity.verify();
    api.polls = [serviceError("signed_out")];

    await tick();
    await tick();

    expect(identity.view).toEqual({ kind: "signedOut" });
    expect(api.pollCount).toBe(1);
  });

  it("stops when disposed", async () => {
    open("processing");
    identity.dispose();

    await tick();

    expect(api.pollCount).toBe(0);
  });
});

describe("Check again", () => {
  it("asks once and shows the answer", async () => {
    open("unverified");
    api.status = "verified";
    const seen: IdentityView[] = [];
    identity.subscribe((v) => seen.push(v));

    await identity.check();

    expect(seen).toEqual([
      { kind: "status", state: "unverified", busy: "checking" },
      { kind: "status", state: "verified" },
    ]);
  });

  it("keeps the state and says why when the check fails", async () => {
    open("requires_input");
    api.polls = [serviceError("offline")];

    await identity.check();

    expect(identity.view).toEqual({ kind: "status", state: "requires_input", error: "offline" });
  });
});
