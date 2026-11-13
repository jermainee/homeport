import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SignIn, errorCode, type LoginStart, type State } from "./signin";
import { FakeAuth, member, serviceError } from "./test/fake-auth";

let auth: FakeAuth;
let signIn: SignIn;

beforeEach(() => {
  vi.useFakeTimers();
  auth = new FakeAuth();
  signIn = new SignIn(auth);
});

afterEach(() => {
  signIn.dispose();
  vi.useRealTimers();
});

describe("at app start", () => {
  it("asks for an email when nobody is signed in", async () => {
    await signIn.boot();

    expect(signIn.state).toEqual({ kind: "signedOut", email: "" });
  });

  it("opens home when this profile is signed in", async () => {
    auth.signedIn = member;

    await signIn.boot();

    expect(signIn.state).toEqual({ kind: "signedIn", member });
  });

  it("offers a retry when Homeport can't be reached", async () => {
    auth.signedIn = member;
    auth.currentError = serviceError("offline");

    await signIn.boot();
    expect(signIn.state).toEqual({ kind: "unreachable", error: "offline" });

    auth.currentError = null;
    await signIn.boot();
    expect(signIn.state).toEqual({ kind: "signedIn", member });
  });
});

describe("entering an email", () => {
  beforeEach(async () => {
    await signIn.boot();
  });

  it("shows the match code while it waits for the member", async () => {
    const seen: string[] = [];
    signIn.subscribe((s) => seen.push(s.kind));

    await signIn.submit("  member@example.com ");

    expect(seen).toEqual(["sending", "waiting"]);
    expect(auth.starts).toEqual(["member@example.com"]);
    expect(signIn.state).toMatchObject({
      kind: "waiting",
      email: "member@example.com",
      matchCode: "482913",
      offline: false,
    });
  });

  it("catches an address without @ before calling Homeport", async () => {
    await signIn.submit("member.example.com");

    expect(auth.starts).toEqual([]);
    expect(signIn.state).toEqual({
      kind: "signedOut",
      email: "member.example.com",
      error: "invalid_email",
    });
  });

  it("keeps the email and explains a failed send", async () => {
    auth.startError = serviceError("rate_limited");

    await signIn.submit("member@example.com");

    expect(signIn.state).toEqual({
      kind: "signedOut",
      email: "member@example.com",
      error: "rate_limited",
    });
  });
});

describe("waiting for the member to approve", () => {
  beforeEach(async () => {
    await signIn.boot();
    await signIn.submit("member@example.com");
  });

  it("checks every 2 seconds and opens home once approved", async () => {
    auth.pollResults = ["pending", "approved"];

    await vi.advanceTimersByTimeAsync(1999);
    expect(auth.polls).toBe(0);
    await vi.advanceTimersByTimeAsync(1);
    expect(auth.polls).toBe(1);
    expect(signIn.state.kind).toBe("waiting");

    await vi.advanceTimersByTimeAsync(2000);
    expect(signIn.state).toEqual({ kind: "signedIn", member });

    await vi.advanceTimersByTimeAsync(10_000);
    expect(auth.polls).toBe(2);
  });

  it("says when Homeport can't be reached and keeps trying, more slowly", async () => {
    auth.pollResults = [serviceError("offline"), serviceError("offline"), "pending"];

    await vi.advanceTimersByTimeAsync(2000);
    expect(signIn.state).toMatchObject({ kind: "waiting", offline: true, matchCode: "482913" });

    await vi.advanceTimersByTimeAsync(2000);
    expect(auth.polls).toBe(1); // backed off to 4 s
    await vi.advanceTimersByTimeAsync(2000);
    expect(auth.polls).toBe(2);

    await vi.advanceTimersByTimeAsync(8000);
    expect(auth.polls).toBe(3);
    expect(signIn.state).toMatchObject({ kind: "waiting", offline: false });
  });

  it("stops when the link expires and can send a new one", async () => {
    auth.pollResults = ["expired"];

    await vi.advanceTimersByTimeAsync(2000);
    expect(signIn.state).toEqual({ kind: "expired", email: "member@example.com" });
    await vi.advanceTimersByTimeAsync(10_000);
    expect(auth.polls).toBe(1);

    await signIn.resend();
    expect(auth.starts).toEqual(["member@example.com", "member@example.com"]);
    expect(signIn.state).toMatchObject({ kind: "waiting", matchCode: "482914" });
  });

  it("treats the link as expired once its time is up, even offline", async () => {
    auth.pollResults = Array.from({ length: 200 }, () => serviceError("offline"));

    await vi.advanceTimersByTimeAsync(15 * 60_000);

    expect(signIn.state).toEqual({ kind: "expired", email: "member@example.com" });
  });

  it("goes back to the email, which can be changed", async () => {
    await signIn.useDifferentEmail();

    expect(auth.cancels).toBe(1);
    expect(signIn.state).toEqual({ kind: "signedOut", email: "member@example.com" });
    await vi.advanceTimersByTimeAsync(10_000);
    expect(auth.polls).toBe(0);
  });

  it("ignores a poll answer that arrives after going back", async () => {
    let answer!: (r: { status: "approved" }) => void;
    auth.poll = () => {
      auth.polls++;
      return new Promise((resolve) => (answer = resolve));
    };
    await vi.advanceTimersByTimeAsync(2000);

    await signIn.useDifferentEmail();
    answer({ status: "approved" });
    await vi.advanceTimersByTimeAsync(0);

    expect(signIn.state.kind).toBe("signedOut");
  });
});

describe("signed in", () => {
  it("signs out and offers the same email again", async () => {
    auth.signedIn = member;
    await signIn.boot();

    await signIn.signOut();

    expect(auth.signOuts).toBe(1);
    expect(signIn.state).toEqual({ kind: "signedOut", email: "member@example.com" });
  });
});

describe("sending a new link", () => {
  beforeEach(async () => {
    await signIn.boot();
    await signIn.submit("member@example.com");
  });

  it("stays on the waiting screen and shows the new code", async () => {
    const seen: State[] = [];
    signIn.subscribe((s) => seen.push(s));

    await signIn.resend();

    expect(seen.map((s) => s.kind)).toEqual(["waiting", "waiting"]);
    expect(seen[0]).toMatchObject({ matchCode: "482913", resending: true });
    expect(signIn.state).toMatchObject({ kind: "waiting", matchCode: "482914", resending: false });
  });

  it("keeps waiting for the first link when the new one can't be sent", async () => {
    auth.startError = serviceError("rate_limited");

    await signIn.resend();

    expect(signIn.state).toMatchObject({
      kind: "waiting",
      matchCode: "482913",
      resending: false,
      error: "rate_limited",
    });
    auth.pollResults = ["approved"];
    await vi.advanceTimersByTimeAsync(2000);
    expect(signIn.state).toEqual({ kind: "signedIn", member });
  });

  it("drops the answer when the member goes back meanwhile", async () => {
    let answer!: (l: LoginStart) => void;
    auth.start = (email) => {
      auth.starts.push(email);
      return new Promise((resolve) => (answer = resolve));
    };
    const resent = signIn.resend();

    await signIn.useDifferentEmail();
    answer({ email: "member@example.com", matchCode: "999999", expiresAt: new Date(Date.now() + 60_000).toISOString() });
    await resent;

    expect(signIn.state).toEqual({ kind: "signedOut", email: "member@example.com" });
  });
});

describe("errorCode", () => {
  it("reads the code from a Go service error and falls back to unexpected", () => {
    expect(errorCode(serviceError("clock_skew"))).toBe("clock_skew");
    expect(errorCode(serviceError("something_new"))).toBe("unexpected");
    expect(errorCode(new TypeError("boom"))).toBe("unexpected");
  });
});
