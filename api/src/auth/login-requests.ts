import { randomMatchCode, randomToken, sha256Hex } from "../crypto";
import { SESSION_TTL_MS } from "./sessions";

/** How long a sign-in link, and the login request behind it, stays valid. */
export const LOGIN_REQUEST_TTL_MS = 15 * 60 * 1000;

/**
 * How long polls keep answering "approved" after the member approves, so a
 * confirmation just before the link expires still reaches the app.
 */
export const APPROVED_POLL_WINDOW_MS = 5 * 60 * 1000;

export interface NewLoginRequest {
  id: string;
  matchCode: string;
  linkToken: string;
  expiresAt: number;
}

/**
 * Starts a sign-in for the session token whose SHA-256 the app sent. Returns
 * null when that token already belongs to another login request or session.
 */
export async function createLoginRequest(
  db: D1Database,
  input: { email: string; sessionTokenHash: string; now: number },
): Promise<NewLoginRequest | null> {
  const id = crypto.randomUUID();
  const linkToken = randomToken();
  const matchCode = randomMatchCode();
  const expiresAt = input.now + LOGIN_REQUEST_TTL_MS;

  const [created] = await db.batch([
    db
      .prepare(
        `INSERT INTO login_requests (id, email, session_token_hash, match_code, created_at, expires_at)
         SELECT ?, ?, ?, ?, ?, ?
          WHERE NOT EXISTS (SELECT 1 FROM auth_sessions WHERE token_hash = ?)
         ON CONFLICT (session_token_hash) DO NOTHING`,
      )
      .bind(
        id,
        input.email,
        input.sessionTokenHash,
        matchCode,
        input.now,
        expiresAt,
        input.sessionTokenHash,
      ),
    db
      .prepare(
        `INSERT INTO magic_link_tokens (token_hash, login_request_id, created_at, expires_at)
         SELECT ?, id, ?, ? FROM login_requests WHERE id = ?`,
      )
      .bind(await sha256Hex(linkToken), input.now, expiresAt, id),
  ]);
  if (created?.meta.changes !== 1) return null;
  return { id, matchCode, linkToken, expiresAt };
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
  return findPendingLinkByHash(db, await sha256Hex(token), now);
}

async function findPendingLinkByHash(
  db: D1Database,
  tokenHash: string,
  now: number,
): Promise<PendingLink | null> {
  const row = await db
    .prepare(
      `SELECT r.id, r.email, r.match_code
         FROM magic_link_tokens t JOIN login_requests r ON r.id = t.login_request_id
        WHERE t.token_hash = ? AND t.used_at IS NULL AND t.expires_at > ?`,
    )
    .bind(tokenHash, now)
    .first<{ id: string; email: string; match_code: string }>();
  return row ? { loginRequestId: row.id, email: row.email, matchCode: row.match_code } : null;
}

export type ConfirmResult = "confirmed" | "invalid_link" | "code_mismatch";

/**
 * Approves a login request from its emailed link. The link token is used up
 * only when the match code agrees.
 */
export async function confirmLink(
  db: D1Database,
  input: { token: string; matchCode: string; now: number },
): Promise<ConfirmResult> {
  const tokenHash = await sha256Hex(input.token);
  const link = await findPendingLinkByHash(db, tokenHash, input.now);
  if (!link) return "invalid_link";
  if (!timingSafeEqual(input.matchCode, link.matchCode)) return "code_mismatch";

  // The guarded UPDATE is the single point that makes a link single-use.
  const used = await db
    .prepare(
      `UPDATE magic_link_tokens SET used_at = ?
        WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
    )
    .bind(input.now, tokenHash, input.now)
    .run();
  if (used.meta.changes !== 1) return "invalid_link";

  await approve(db, { loginRequestId: link.loginRequestId, email: link.email, now: input.now });
  return "confirmed";
}

/**
 * Approves a login request with its session token instead of the emailed
 * link. Only the dev routes call this, so automated tests can sign in.
 */
export async function approveWithSessionToken(
  db: D1Database,
  input: { id: string; sessionToken: string; now: number },
): Promise<"approved" | "not_found" | "expired"> {
  const request = await findRequestByToken(db, input.id, input.sessionToken);
  if (!request) return "not_found";
  if (request.expiresAt <= input.now) return "expired";
  await approve(db, { loginRequestId: input.id, email: request.email, now: input.now });
  return "approved";
}

/**
 * Creates the member on first sign-in, ties the login request to them and
 * activates its session token, in one transaction. The request stays open for
 * at least APPROVED_POLL_WINDOW_MS so the app's poll sees the approval.
 */
async function approve(
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
                expires_at = MAX(expires_at, ?),
                member_id = (SELECT m.id FROM members m WHERE m.email = login_requests.email)
          WHERE id = ? AND confirmed_at IS NULL`,
      )
      .bind(input.now, input.now + APPROVED_POLL_WINDOW_MS, input.loginRequestId),
    // A session that already exists (approved before, maybe signed out since)
    // is left as it is.
    db
      .prepare(
        `INSERT INTO auth_sessions (token_hash, member_id, created_at, expires_at)
         SELECT session_token_hash, member_id, ?, ?
           FROM login_requests WHERE id = ? AND confirmed_at IS NOT NULL
         ON CONFLICT (token_hash) DO NOTHING`,
      )
      .bind(input.now, input.now + SESSION_TTL_MS, input.loginRequestId),
  ]);
}

export type PollResult =
  | { status: "not_found" }
  | { status: "pending" }
  | { status: "expired" }
  | { status: "approved"; memberId: string; sessionExpiresAt: number };

/**
 * What the app sees when it polls its login request with its session token.
 * Once approved, every poll answers approved until the request expires.
 */
export async function pollLoginRequest(
  db: D1Database,
  input: { id: string; sessionToken: string; now: number },
): Promise<PollResult> {
  const request = await findRequestByToken(db, input.id, input.sessionToken);
  if (!request) return { status: "not_found" };
  if (request.expiresAt <= input.now) return { status: "expired" };
  if (request.memberId === null || request.sessionExpiresAt === null) return { status: "pending" };
  return {
    status: "approved",
    memberId: request.memberId,
    sessionExpiresAt: request.sessionExpiresAt,
  };
}

interface StoredRequest {
  email: string;
  expiresAt: number;
  memberId: string | null;
  sessionExpiresAt: number | null;
}

/** The login request with this id, if `sessionToken` is the token it was started with. */
async function findRequestByToken(
  db: D1Database,
  id: string,
  sessionToken: string,
): Promise<StoredRequest | null> {
  const row = await db
    .prepare(
      `SELECT r.email, r.session_token_hash, r.expires_at, r.member_id,
              s.expires_at AS session_expires_at
         FROM login_requests r LEFT JOIN auth_sessions s ON s.token_hash = r.session_token_hash
        WHERE r.id = ?`,
    )
    .bind(id)
    .first<{
      email: string;
      session_token_hash: string;
      expires_at: number;
      member_id: string | null;
      session_expires_at: number | null;
    }>();
  if (!row || !timingSafeEqual(await sha256Hex(sessionToken), row.session_token_hash)) return null;
  return {
    email: row.email,
    expiresAt: row.expires_at,
    memberId: row.member_id,
    sessionExpiresAt: row.session_expires_at,
  };
}

function timingSafeEqual(a: string, b: string): boolean {
  const encoder = new TextEncoder();
  const left = encoder.encode(a);
  const right = encoder.encode(b);
  return left.byteLength === right.byteLength && crypto.subtle.timingSafeEqual(left, right);
}
