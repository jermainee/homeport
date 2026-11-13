// The sign-in flow as a state machine. It drives the Go AuthService through
// the AuthApi seam and knows nothing about the DOM, so it is tested without
// a webview.

export interface Credits {
  purchased: number;
  earnedPending: number;
  earnedCashable: number;
}

export interface Member {
  memberId: string;
  email: string;
  identityStatus: string;
  credits: Credits;
}

export interface LoginStart {
  email: string;
  matchCode: string;
  expiresAt: string; // RFC 3339
}

export type LoginStatus = "pending" | "approved" | "expired";

/** What the Go AuthService offers the webview. */
export interface AuthApi {
  current(): Promise<Member | null>;
  start(email: string): Promise<LoginStart>;
  poll(): Promise<{ status: LoginStatus }>;
  cancel(): Promise<void>;
  signOut(): Promise<void>;
}

/** Error codes from services.ErrorCode in Go, plus a local "invalid_email". */
export type ErrorCode =
  | "offline"
  | "invalid_email"
  | "rate_limited"
  | "email_unavailable"
  | "no_sign_in"
  | "unexpected";

export type State =
  | { kind: "checking" }
  | { kind: "unreachable"; error: ErrorCode }
  | { kind: "signedOut"; email: string; error?: ErrorCode }
  | { kind: "sending"; email: string }
  | {
      kind: "waiting";
      email: string;
      matchCode: string;
      expiresAt: string;
      offline: boolean;
    }
  | { kind: "expired"; email: string }
  | { kind: "signedIn"; member: Member }
  | { kind: "signingOut"; member: Member };

const errorCodes: readonly ErrorCode[] = [
  "offline",
  "invalid_email",
  "rate_limited",
  "email_unavailable",
  "no_sign_in",
  "unexpected",
];

/** errorCode reads the code a Go service error carries in its cause. */
export function errorCode(err: unknown): ErrorCode {
  const code = (err as { cause?: { code?: unknown } } | null)?.cause?.code;
  return errorCodes.includes(code as ErrorCode) ? (code as ErrorCode) : "unexpected";
}

// A light check that catches typos before a round trip; the API decides.
function looksLikeEmail(email: string): boolean {
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email);
}

/** How often to ask whether the member approved, and the slowest retry. */
export const POLL_EVERY_MS = 2000;
export const MAX_BACKOFF_MS = 15_000;

export class SignIn {
  #state: State = { kind: "checking" };
  #listeners = new Set<(s: State) => void>();
  #timer: ReturnType<typeof setTimeout> | undefined;
  #run = 0; // bumped whenever polling stops, so late answers are dropped
  #failures = 0;

  constructor(private readonly api: AuthApi) {}

  get state(): State {
    return this.#state;
  }

  /** subscribe calls fn on every state change and returns an unsubscribe. */
  subscribe(fn: (s: State) => void): () => void {
    this.#listeners.add(fn);
    return () => this.#listeners.delete(fn);
  }

  /** boot finds out whether this profile is signed in. */
  async boot(): Promise<void> {
    this.#set({ kind: "checking" });
    await this.#loadMember();
  }

  async #loadMember(): Promise<void> {
    try {
      const member = await this.api.current();
      this.#set(member ? { kind: "signedIn", member } : { kind: "signedOut", email: "" });
    } catch (err) {
      this.#set({ kind: "unreachable", error: errorCode(err) });
    }
  }

  /** submit emails a sign-in link to email. */
  async submit(email: string): Promise<void> {
    email = email.trim();
    if (!looksLikeEmail(email)) {
      this.#set({ kind: "signedOut", email, error: "invalid_email" });
      return;
    }
    this.#set({ kind: "sending", email });
    try {
      const login = await this.api.start(email);
      this.#set({
        kind: "waiting",
        email,
        matchCode: login.matchCode,
        expiresAt: login.expiresAt,
        offline: false,
      });
      this.#failures = 0;
      this.#schedule(POLL_EVERY_MS);
    } catch (err) {
      this.#set({ kind: "signedOut", email, error: errorCode(err) });
    }
  }

  /** resend emails a new link to the same address. */
  async resend(): Promise<void> {
    const s = this.#state;
    if (s.kind !== "waiting" && s.kind !== "expired") return;
    this.#stop();
    await this.submit(s.email);
  }

  /** useDifferentEmail abandons the sign-in and goes back to the email. */
  async useDifferentEmail(): Promise<void> {
    const s = this.#state;
    if (s.kind !== "waiting" && s.kind !== "expired") return;
    this.#stop();
    this.#set({ kind: "signedOut", email: s.email });
    await this.api.cancel();
  }

  /** signOut ends the session and goes back to the email. */
  async signOut(): Promise<void> {
    const s = this.#state;
    if (s.kind !== "signedIn") return;
    this.#set({ kind: "signingOut", member: s.member });
    try {
      await this.api.signOut();
      this.#set({ kind: "signedOut", email: s.member.email });
    } catch {
      this.#set({ kind: "signedIn", member: s.member });
    }
  }

  /** dispose stops polling. */
  dispose(): void {
    this.#stop();
  }

  #stop(): void {
    clearTimeout(this.#timer);
    this.#timer = undefined;
    this.#run++;
  }

  // #schedule polls after delay, or flips to expired when the link runs out
  // first, so an unreachable API can't keep the member waiting forever.
  #schedule(delay: number): void {
    const s = this.#state;
    if (s.kind !== "waiting") return;
    const run = ++this.#run;
    const left = Date.parse(s.expiresAt) - Date.now();
    this.#timer = setTimeout(
      () => {
        if (run !== this.#run) return;
        if (Date.parse(s.expiresAt) <= Date.now()) {
          this.#stop();
          this.#set({ kind: "expired", email: s.email });
          return;
        }
        void this.#poll(run);
      },
      Math.max(0, Math.min(delay, left)),
    );
  }

  async #poll(run: number): Promise<void> {
    let status: LoginStatus;
    try {
      ({ status } = await this.api.poll());
    } catch (err) {
      if (run !== this.#run) return;
      this.#pollFailed(errorCode(err));
      return;
    }
    if (run !== this.#run) return;
    const s = this.#state;
    if (s.kind !== "waiting") return;

    switch (status) {
      case "pending":
        this.#failures = 0;
        if (s.offline) this.#set({ ...s, offline: false });
        this.#schedule(POLL_EVERY_MS);
        return;
      case "expired":
        this.#stop();
        this.#set({ kind: "expired", email: s.email });
        return;
      case "approved":
        this.#stop();
        await this.#loadMember();
        return;
    }
  }

  #pollFailed(code: ErrorCode): void {
    const s = this.#state;
    if (s.kind !== "waiting") return;
    if (code === "no_sign_in") {
      // The Go side dropped the sign-in (the API refused the token).
      this.#stop();
      this.#set({ kind: "signedOut", email: s.email, error: "unexpected" });
      return;
    }
    this.#failures++;
    const offline = code === "offline";
    if (s.offline !== offline) this.#set({ ...s, offline });
    this.#schedule(Math.min(POLL_EVERY_MS * 2 ** this.#failures, MAX_BACKOFF_MS));
  }

  #set(s: State): void {
    this.#state = s;
    for (const fn of this.#listeners) fn(s);
  }
}
