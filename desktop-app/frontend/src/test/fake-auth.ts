// A stand-in for the Go AuthService behind the Wails bindings, for tests.

import type { AuthApi, LoginStart, Member } from "../signin";

export const member: Member = {
  memberId: "01a1c4af-8795-7a28-9249-adbd2ba1c04a",
  email: "member@example.com",
  identityStatus: "unverified",
  credits: { purchased: 0, earnedPending: 0, earnedCashable: 0 },
};

// serviceError is how a Go service error reaches the webview: Wails rejects
// the call with an Error whose cause is the marshalled *services.Error.
export function serviceError(code: string): Error {
  return Object.assign(new Error(`${code}: details for the log`), { cause: { code } });
}

export class FakeAuth implements AuthApi {
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
