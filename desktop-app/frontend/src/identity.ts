// The identity check as a state machine, shared by Share and Use: every
// member passes Stripe Identity once before either turns on. It drives the
// Go IdentityService through the IdentityApi seam and knows nothing about
// the DOM. Go opens Stripe's page in the browser; this follows the check
// until it has an outcome.

import { startPolling } from "./poll";
import { errorCode, type ErrorCode } from "./signin";

/** services.IdentityState in Go. */
export type IdentityState = "unverified" | "waiting" | "processing" | "requires_input" | "verified";

export const IDENTITY_STATES: readonly IdentityState[] = [
  "unverified",
  "waiting",
  "processing",
  "requires_input",
  "verified",
];

/** What the Go IdentityService offers the webview. */
export interface IdentityApi {
  /** start opens a new check in the browser. */
  start(): Promise<IdentityState>;
  /** poll asks once how the check stands. */
  poll(): Promise<IdentityState>;
}

export type IdentityView =
  | {
      kind: "status";
      state: IdentityState;
      /** starting while Verify identity opens the check; checking while Check again asks. */
      busy?: "starting" | "checking";
      /** Homeport can't be reached; polling keeps trying. */
      offline?: boolean;
      /** Polling stopped waiting; Check again asks once more. */
      stalled?: boolean;
      /** Why the last press didn't work. */
      error?: ErrorCode;
    }
  | { kind: "signedOut" };

/** How often to ask how the check stands, and how long to keep asking. */
export const IDENTITY_POLL_MS = 3000;
export const IDENTITY_GIVE_UP_MS = 30 * 60_000;

// The states in which Stripe hasn't reported an outcome yet.
const inProgress = (s: IdentityState) => s === "waiting" || s === "processing";

export function identityState(s: string): IdentityState {
  return (IDENTITY_STATES as readonly string[]).includes(s) ? (s as IdentityState) : "unverified";
}

export class Identity {
  #view: IdentityView;
  #listeners = new Set<(v: IdentityView) => void>();
  #stopPolling: (() => void) | undefined;
  #disposed = false;

  /** status is the member's identity_status when the screen opened. */
  constructor(
    private readonly api: IdentityApi,
    status: string,
  ) {
    this.#view = { kind: "status", state: identityState(status) };
  }

  get view(): IdentityView {
    return this.#view;
  }

  get verified(): boolean {
    return this.#view.kind === "status" && this.#view.state === "verified";
  }

  /** subscribe calls fn on every view change and returns an unsubscribe. */
  subscribe(fn: (v: IdentityView) => void): () => void {
    this.#listeners.add(fn);
    return () => this.#listeners.delete(fn);
  }

  /** start follows a check that is already being processed. */
  start(): void {
    const v = this.#view;
    if (v.kind === "status" && inProgress(v.state)) this.#poll();
  }

  /** verify opens a new identity check in the browser and follows it. */
  async verify(): Promise<void> {
    const v = this.#view;
    if (v.kind !== "status" || v.busy) return;
    this.#stop();
    this.#set({ kind: "status", state: v.state, busy: "starting" });
    let state: IdentityState;
    try {
      state = await this.api.start();
    } catch (err) {
      this.#failed(v.state, errorCode(err));
      return;
    }
    this.#show(state);
  }

  /** check asks once how the check stands, as "Check again" does. */
  async check(): Promise<void> {
    const v = this.#view;
    if (v.kind !== "status" || v.busy) return;
    this.#stop();
    this.#set({ kind: "status", state: v.state, busy: "checking" });
    let state: IdentityState;
    try {
      state = await this.api.poll();
    } catch (err) {
      this.#failed(v.state, errorCode(err));
      return;
    }
    this.#show(state);
  }

  /** dispose stops polling and drops answers still on their way. */
  dispose(): void {
    this.#disposed = true;
    this.#stop();
  }

  // #show shows a state from Go and follows it while it is in progress.
  #show(state: IdentityState): void {
    if (this.#disposed) return;
    this.#set({ kind: "status", state });
    if (inProgress(state)) this.#poll();
  }

  #failed(state: IdentityState, error: ErrorCode): void {
    if (this.#disposed) return;
    this.#set(error === "signed_out" ? { kind: "signedOut" } : { kind: "status", state, error });
  }

  #poll(): void {
    this.#stop();
    const stop = startPolling({
      every: IDENTITY_POLL_MS,
      giveUpAfter: IDENTITY_GIVE_UP_MS,
      tick: async (live) => {
        const state = await this.api.poll();
        if (!live() || this.#disposed) return false;
        this.#set({ kind: "status", state });
        return inProgress(state);
      },
      failed: (code) => {
        const v = this.#view;
        if (this.#disposed || v.kind !== "status") return false;
        if (code === "signed_out") {
          this.#set({ kind: "signedOut" });
          return false;
        }
        this.#set({ kind: "status", state: v.state, offline: code === "offline" || undefined });
        return true;
      },
      gaveUp: () => {
        const v = this.#view;
        if (!this.#disposed && v.kind === "status") this.#set({ kind: "status", state: v.state, stalled: true });
      },
    });
    this.#stopPolling = stop;
  }

  #stop(): void {
    this.#stopPolling?.();
    this.#stopPolling = undefined;
  }

  #set(v: IdentityView): void {
    if (sameView(this.#view, v)) return;
    this.#view = v;
    for (const fn of this.#listeners) fn(v);
  }
}

function sameView(a: IdentityView, b: IdentityView): boolean {
  if (a.kind !== b.kind) return false;
  if (a.kind === "signedOut" || b.kind === "signedOut") return true;
  return a.state === b.state && a.busy === b.busy && a.offline === b.offline && a.stalled === b.stalled && a.error === b.error;
}
