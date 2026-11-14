// A stand-in for the Go IdentityService behind the Wails bindings, for tests.

import type { IdentityApi, IdentityState } from "../identity";

export class FakeIdentity implements IdentityApi {
  /** What Start answers, or the error it throws. */
  startAnswer: IdentityState | Error = "waiting";
  /** What the next polls answer, in order; after that, `status`. */
  polls: Array<IdentityState | Error> = [];
  /** What a poll answers once `polls` is used up. */
  status: IdentityState = "waiting";
  starts = 0;
  pollCount = 0;

  async start() {
    this.starts++;
    const a = this.startAnswer;
    if (a instanceof Error) throw a;
    return a;
  }
  async poll() {
    this.pollCount++;
    const next = this.polls.shift() ?? this.status;
    if (next instanceof Error) throw next;
    return next;
  }
}
