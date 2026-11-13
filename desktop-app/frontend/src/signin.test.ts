import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SignIn, type AuthApi, type LoginStart, type Member } from "./signin";

const member: Member = {
  memberId: "01a1c4af-8795-7a28-9249-adbd2ba1c04a",
  email: "member@example.com",
  identityStatus: "unverified",
  credits: { purchased: 0, earnedPending: 0, earnedCashable: 0 },
};

// serviceError is how a Go service error reaches the webview: Wails rejects
// the call with an Error whose cause is the marshalled *services.Error.
function serviceError(code: string): Error {
  return Object.assign(new Error(`${code}: details for the log`), { cause: { code } });
}

// FakeAuth stands in for the Go AuthService behind the Wails bindings.
class FakeAuth implements AuthApi {
  signedIn: Member | null = null;
  currentError: Error | null = null;
  startError: Error | null = null;
  pollResults: Array<"pending" | "approved" | "expired" | Error> = [];
  starts: string[] = [];
  polls = 0;
  cancels = 0;
  signOuts = 0;
  code = 482913;

  async current() {
    if (this.currentError) throw this.currentError;
    return this.signedIn;
  }
  async start(email: string): Promise<LoginStart> {
    this.starts.push(email);
    if (this.startError) throw this.startError;
    return {
      email,
      matchCode: String(this.code++),
      expiresAt: new Date(Date.now() + 15 * 60_000).toISOString(),
    };
  }
  async poll() {
    this.polls++;
    const next = this.pollResults.shift() ?? "pending";
    if (next instanceof Error) throw next;
    if (next === "approved") this.signedIn = member;
    return { status: next };
  }
  async cancel() {
    this.cancels++;
  }
  async signOut() {
    this.signOuts++;
    this.signedIn = null;
  }
}

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
