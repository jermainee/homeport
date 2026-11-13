// A stand-in for the Go ShareService behind the Wails bindings, for tests.

import type { ShareApi, ShareStatus } from "../share";

export const off: ShareStatus = { state: "off", sharing: false, bytesToday: 0, flowsActive: 0 };

export class FakeShare implements ShareApi {
  current: ShareStatus = off;
  identityStatus = "verified";
  /** What SetSharing answers next, or the error it throws. */
  answer: ShareStatus | Error | null = null;
  toggles: boolean[] = [];
  identityChecks = 0;
  #listeners = new Set<(s: ShareStatus) => void>();
  #hold: Promise<void> | null = null;
  #release: (() => void) | null = null;

  async status() {
    return this.current;
  }
  async setSharing(on: boolean) {
    this.toggles.push(on);
    await this.#hold;
    const a = this.answer ?? { ...this.current, sharing: on, state: on ? "connecting" : "off" };
    this.answer = null;
    if (a instanceof Error) throw a;
    this.current = a;
    return a;
  }
  onStatus(fn: (s: ShareStatus) => void) {
    this.#listeners.add(fn);
    return () => void this.#listeners.delete(fn);
  }
  async identity() {
    this.identityChecks++;
    return this.identityStatus;
  }

  /** emit is Go publishing a status change. */
  emit(s: Partial<ShareStatus>) {
    this.current = { ...this.current, ...s };
    for (const fn of this.#listeners) fn(this.current);
  }
  get listening() {
    return this.#listeners.size;
  }
  /** hold keeps SetSharing waiting until release. */
  hold() {
    this.#hold = new Promise((r) => (this.#release = r));
  }
  release() {
    this.#release?.();
    this.#hold = null;
  }
}
