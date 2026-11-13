import { randomMatchCode, randomToken, sha256Hex } from "../crypto";
import { newSession } from "./sessions";

/** How long a sign-in link, and the login request behind it, stays valid. */
export const LOGIN_REQUEST_TTL_MS = 15 * 60 * 1000;

export interface NewLoginRequest {
  id: string;
  pollSecret: string;
  matchCode: string;
  linkToken: string;
  expiresAt: number;
}

export async function createLoginRequest(
  db: D1Database,
  input: { email: string; installId: string | null; now: number },
): Promise<NewLoginRequest> {
  const id = crypto.randomUUID();
  const pollSecret = randomToken();
  const linkToken = randomToken();
  const matchCode = randomMatchCode();
  const expiresAt = input.now + LOGIN_REQUEST_TTL_MS;

  await db.batch([
    db
      .prepare(
        `INSERT INTO login_requests
           (id, email, poll_secret_hash, match_code, install_id, created_at, expires_at)
         VALUES (?, ?, ?, ?, ?, ?, ?)`,
      )
      .bind(id, input.email, await sha256Hex(pollSecret), matchCode, input.installId, input.now, expiresAt),
    db
      .prepare(
        `INSERT INTO magic_link_tokens (token_hash, login_request_id, created_at, expires_at)
         VALUES (?, ?, ?, ?)`,
      )
      .bind(await sha256Hex(linkToken), id, input.now, expiresAt),
  ]);

  return { id, pollSecret, matchCode, linkToken, expiresAt };
}

export interface PendingLink {
  loginRequestId: string;
  email: string;
  matchCode: string;
}

/** The login request behind an unused, unexpired link token, or null. */
export async function findPendingLink(
  db: D1Database,
  token: string,
  now: number,
): Promise<PendingLink | null> {
  const row = await db
    .prepare(
      `SELECT r.id, r.email, r.match_code
         FROM magic_link_tokens t JOIN login_requests r ON r.id = t.login_request_id
        WHERE t.token_hash = ? AND t.used_at IS NULL AND t.expires_at > ?`,
    )
    .bind(await sha256Hex(token), now)
    .first<{ id: string; email: string; match_code: string }>();
  return row ? { loginRequestId: row.id, email: row.email, matchCode: row.match_code } : null;
}

export type ConfirmResult = "confirmed" | "invalid_link" | "code_mismatch";

/**
 * Confirms a login request from its emailed link. The link token is used up
 * only when the match code agrees, and the member is created on first sign-in.
 */
export async function confirmLink(
  db: D1Database,
  input: { token: string; matchCode: string; now: number },
): Promise<ConfirmResult> {
  const link = await findPendingLink(db, input.token, input.now);
  if (!link) return "invalid_link";
  if (!timingSafeEqual(input.matchCode, link.matchCode)) return "code_mismatch";

  // The guarded UPDATE is the single point that makes a link single-use.
  const used = await db
    .prepare(
      `UPDATE magic_link_tokens SET used_at = ?
        WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
    )
    .bind(input.now, await sha256Hex(input.token), input.now)
    .run();
  if (used.meta.changes !== 1) return "invalid_link";

  await markConfirmed(db, { loginRequestId: link.loginRequestId, email: link.email, now: input.now });
  return "confirmed";
}

/**
 * Confirms a login request with its poll secret instead of the emailed link.
 * Only the dev routes call this, so automated tests can sign in without email.
 */
export async function confirmWithPollSecret(
  db: D1Database,
  input: { id: string; pollSecret: string; now: number },
): Promise<"confirmed" | "not_found" | "expired"> {
  const row = await db
    .prepare(`SELECT email, poll_secret_hash, expires_at FROM login_requests WHERE id = ?`)
    .bind(input.id)
    .first<{ email: string; poll_secret_hash: string; expires_at: number }>();
  if (!row || !(await secretMatchesHash(input.pollSecret, row.poll_secret_hash))) return "not_found";
  if (row.expires_at <= input.now) return "expired";
  await markConfirmed(db, { loginRequestId: input.id, email: row.email, now: input.now });
  return "confirmed";
}

/** Creates the member on first sign-in and ties the login request to them. */
async function markConfirmed(
  db: D1Database,
  input: { loginRequestId: string; email: string; now: number },
): Promise<void> {
  await db.batch([
    db
      .prepare(
        `INSERT INTO members (id, email, created_at) VALUES (?, ?, ?)
           ON CONFLICT (email) DO NOTHING`,
      )
      .bind(crypto.randomUUID(), input.email, input.now),
    db
      .prepare(
        `UPDATE login_requests
            SET confirmed_at = ?,
                member_id = (SELECT m.id FROM members m WHERE m.email = login_requests.email)
          WHERE id = ? AND confirmed_at IS NULL`,
      )
      .bind(input.now, input.loginRequestId),
  ]);
}

export type PollResult =
  | { status: "not_found" }
  | { status: "pending" }
  | { status: "expired" }
  | { status: "confirmed"; sessionToken: string; expiresAt: number };

/**
 * What the app sees when it polls its login request with the poll secret.
 * A confirmed request hands out its session token exactly once.
 */
export async function pollLoginRequest(
  db: D1Database,
  input: { id: string; pollSecret: string; now: number },
): Promise<PollResult> {
  const row = await db
    .prepare(
      `SELECT poll_secret_hash, expires_at, confirmed_at, completed_at
         FROM login_requests WHERE id = ?`,
    )
    .bind(input.id)
    .first<{
      poll_secret_hash: string;
      expires_at: number;
      confirmed_at: number | null;
      completed_at: number | null;
    }>();
  if (!row || !(await secretMatchesHash(input.pollSecret, row.poll_secret_hash))) {
    return { status: "not_found" };
  }
  if (row.completed_at !== null || row.expires_at <= input.now) return { status: "expired" };
  if (row.confirmed_at === null) return { status: "pending" };

  // One batch is one transaction: the session is inserted only while the
  // request is still uncompleted, and the same batch completes it, so two
  // concurrent polls cannot both get a session.
  const session = await newSession(input.now);
  const [inserted] = await db.batch([
    db
      .prepare(
        `INSERT INTO auth_sessions (token_hash, member_id, install_id, created_at, expires_at)
         SELECT ?, member_id, install_id, ?, ?
           FROM login_requests
          WHERE id = ? AND confirmed_at IS NOT NULL AND completed_at IS NULL AND expires_at > ?`,
      )
      .bind(session.tokenHash, input.now, session.expiresAt, input.id, input.now),
    db
      .prepare(`UPDATE login_requests SET completed_at = ? WHERE id = ? AND completed_at IS NULL`)
      .bind(input.now, input.id),
  ]);
  if (inserted?.meta.changes !== 1) return { status: "expired" };
  return { status: "confirmed", sessionToken: session.token, expiresAt: session.expiresAt };
}

async function secretMatchesHash(secret: string, expectedHash: string): Promise<boolean> {
  return timingSafeEqual(await sha256Hex(secret), expectedHash);
}

function timingSafeEqual(a: string, b: string): boolean {
  const encoder = new TextEncoder();
  const left = encoder.encode(a);
  const right = encoder.encode(b);
  return left.byteLength === right.byteLength && crypto.subtle.timingSafeEqual(left, right);
}
