// The Share screen as a state machine. It drives the Go ShareService through
// the ShareApi seam and knows nothing about the DOM, so it is tested without
// a webview. Go is the source of truth: it publishes every status change,
// including those the tray makes.

import { errorCode, type ErrorCode } from "./signin";

export type ShareState = "off" | "connecting" | "online" | "paused" | "offline" | "error";

/** services.ShareStatus in Go. */
export interface ShareStatus {
  state: ShareState;
  /** The switch: whether the member wants Share on. */
  sharing: boolean;
  /** Why Share is offline or stopped. */
  error?: ErrorCode;
  bytesToday: number;
  flowsActive: number;
}

/** What the Go ShareService offers the webview. */
export interface ShareApi {
  status(): Promise<ShareStatus>;
  setSharing(on: boolean): Promise<ShareStatus>;
  /** onStatus calls fn on every change Go publishes; it returns an unsubscribe. */
  onStatus(fn: (s: ShareStatus) => void): () => void;
  /** identity asks the API for the member's identity status again; null when nobody is signed in. */
  identity(): Promise<string | null>;
}

export type ShareView =
  | { kind: "loading" }
  // Share needs a verified identity; checking while "Check again" runs.
  | { kind: "verify"; identity: string; checking: boolean }
  // turning while the switch's answer is on its way; failed if it didn't come.
  | { kind: "ready"; status: ShareStatus; turning: boolean; failed?: ErrorCode }
  // The session is gone; the window goes back to sign-in.
  | { kind: "signedOut" };

export class Share {
  #view: ShareView = { kind: "loading" };
  #listeners = new Set<(v: ShareView) => void>();
  #unsubscribe: (() => void) | undefined;
  #disposed = false;

  constructor(
    private readonly api: ShareApi,
    private identity: string,
  ) {}

  get view(): ShareView {
    return this.#view;
  }

  /** subscribe calls fn on every view change and returns an unsubscribe. */
  subscribe(fn: (v: ShareView) => void): () => void {
    this.#listeners.add(fn);
    return () => this.#listeners.delete(fn);
  }

  /** start shows Share as Go has it, or the verify screen. */
  async start(): Promise<void> {
    this.#unsubscribe = this.api.onStatus((s) => this.#follow(s));
    if (this.identity !== "verified") {
      this.#set({ kind: "verify", identity: this.identity, checking: false });
      return;
    }
    await this.#load();
  }

  // #load shows Go's status. After a check found the member verified, an
  // identity_required error Go still holds is from before the check.
  async #load(afterCheck = false): Promise<void> {
    try {
      let status = await this.api.status();
      if (afterCheck && status.error === "identity_required" && !status.sharing) {
        status = { ...status, state: "off", error: undefined };
      }
      this.#follow(status);
    } catch (err) {
      this.#set({ kind: "ready", status: offStatus, turning: false, failed: errorCode(err) });
    }
  }

  /** toggle flips the switch. A press while one is on its way is ignored. */
  async toggle(): Promise<void> {
    const v = this.#view;
    if (v.kind !== "ready" || v.turning) return;
    this.#set({ kind: "ready", status: v.status, turning: true });
    let status: ShareStatus;
    try {
      status = await this.api.setSharing(!v.status.sharing);
    } catch (err) {
      const now = this.#view;
      if (now.kind === "ready") this.#set({ ...now, turning: false, failed: errorCode(err) });
      return;
    }
    this.#follow(status, true);
  }

  /** recheck asks the API again whether the member is verified now. */
  async recheck(): Promise<void> {
    const v = this.#view;
    if (v.kind !== "verify" || v.checking) return;
    this.#set({ ...v, checking: true });
    let identity: string | null;
    try {
      identity = await this.api.identity();
    } catch {
      identity = v.identity;
    }
    if (this.#disposed) return;
    if (identity === null) {
      this.#set({ kind: "signedOut" });
      return;
    }
    this.identity = identity;
    if (identity !== "verified") {
      this.#set({ kind: "verify", identity, checking: false });
      return;
    }
    await this.#load(true);
  }

  /** dispose stops listening to Go. */
  dispose(): void {
    this.#disposed = true;
    this.#unsubscribe?.();
    this.#unsubscribe = undefined;
  }

  // #follow shows a status from Go: published, or the answer to a press,
  // which ends the press.
  #follow(status: ShareStatus, answer = false): void {
    if (this.#disposed) return;
    if (status.error === "signed_out") {
      this.#set({ kind: "signedOut" });
      return;
    }
    if (status.error === "identity_required" && !status.sharing) {
      // Go found the member unverified, so the screen asks them to verify
      // instead of showing a switch that can't turn on.
      this.identity = this.identity === "verified" ? "unverified" : this.identity;
      this.#set({ kind: "verify", identity: this.identity, checking: false });
      return;
    }
    const v = this.#view;
    // Share on means the API accepted the member, for example after they
    // verified and turned Share on from the tray.
    if (status.sharing) this.identity = "verified";
    // Until then, the verify screen stays.
    if (v.kind === "verify" && this.identity !== "verified") return;
    this.#set({ kind: "ready", status, turning: !answer && v.kind === "ready" && v.turning });
  }

  #set(v: ShareView): void {
    this.#view = v;
    for (const fn of this.#listeners) fn(v);
  }
}

const offStatus: ShareStatus = { state: "off", sharing: false, bytesToday: 0, flowsActive: 0 };
