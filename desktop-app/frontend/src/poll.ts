// Polling for the identity check and for a checkout's credits: ask every
// so often, back off while Homeport can't be reached, and give up waiting
// after a while, when the member can still ask by hand.

import { MAX_BACKOFF_MS, errorCode, type ErrorCode } from "./signin";

export interface PollOptions {
  /** How long to wait between polls. */
  every: number;
  /** How long to keep polling before calling gaveUp. */
  giveUpAfter: number;
  /**
   * One poll. It resolves true to poll again and false to stop, and throws
   * to back off. live() turns false once polling stopped, so a late
   * answer is dropped.
   */
  tick(live: () => boolean): Promise<boolean>;
  /** A poll failed with code; return false to stop polling. */
  failed(code: ErrorCode): boolean;
  /** Polling ran out of time. */
  gaveUp(): void;
}

/** startPolling polls after `every`, until told to stop, and returns a stop. */
export function startPolling(o: PollOptions): () => void {
  let stopped = false;
  let failures = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const deadline = Date.now() + o.giveUpAfter;
  const live = () => !stopped;

  const schedule = (delay: number) => {
    if (stopped) return;
    const left = deadline - Date.now();
    if (left <= 0) {
      stopped = true;
      o.gaveUp();
      return;
    }
    timer = setTimeout(run, Math.min(delay, left));
  };

  const run = async () => {
    if (stopped) return;
    let again: boolean;
    try {
      again = await o.tick(live);
    } catch (err) {
      if (stopped) return;
      if (!o.failed(errorCode(err))) {
        stopped = true;
        return;
      }
      failures++;
      schedule(Math.min(o.every * 2 ** failures, MAX_BACKOFF_MS));
      return;
    }
    if (stopped) return;
    failures = 0;
    if (!again) {
      stopped = true;
      return;
    }
    schedule(o.every);
  };

  schedule(o.every);
  return () => {
    stopped = true;
    clearTimeout(timer);
  };
}
