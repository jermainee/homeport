import { randomToken, sha256Hex } from "../crypto";

/** How long a session token from a sign-in stays valid. */
export const SESSION_TTL_MS = 90 * 24 * 60 * 60 * 1000;

export interface Member {
  id: string;
  email: string;
  status: string;
  identityStatus: string;
  createdAt: number;
}

export interface NewSession {
  token: string;
  tokenHash: string;
  expiresAt: number;
}

/** A fresh session token. The caller stores only `tokenHash`. */
export async function newSession(now: number): Promise<NewSession> {
  const token = randomToken();
  return { token, tokenHash: await sha256Hex(token), expiresAt: now + SESSION_TTL_MS };
}

/** The member behind a live (unexpired, unrevoked) session token, or null. */
export async function authenticate(db: D1Database, token: string, now: number): Promise<Member | null> {
  const row = await db
    .prepare(
      `SELECT m.id, m.email, m.status, m.identity_status, m.created_at
         FROM auth_sessions s JOIN members m ON m.id = s.member_id
        WHERE s.token_hash = ? AND s.revoked_at IS NULL AND s.expires_at > ?`,
    )
    .bind(await sha256Hex(token), now)
    .first<{ id: string; email: string; status: string; identity_status: string; created_at: number }>();
  if (!row) return null;
  return {
    id: row.id,
    email: row.email,
    status: row.status,
    identityStatus: row.identity_status,
    createdAt: row.created_at,
  };
}

/** Revokes one session token. The member's other sessions stay signed in. */
export async function revokeSession(db: D1Database, token: string, now: number): Promise<void> {
  await db
    .prepare(`UPDATE auth_sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`)
    .bind(now, await sha256Hex(token))
    .run();
}
