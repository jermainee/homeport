import { sha256Hex } from "../crypto";

/** How long a session token stays active after the member approves the sign-in. */
export const SESSION_TTL_MS = 90 * 24 * 60 * 60 * 1000;

/** A session token as the app creates it: `hpm_` and 32 random bytes in base64url. */
export const SESSION_TOKEN = /^hpm_[A-Za-z0-9_-]{43}$/;

export interface Member {
  id: string;
  email: string;
  identityStatus: string;
}

/** The member behind a live (approved, unexpired, unrevoked) session token, or null. */
export async function authenticate(db: D1Database, token: string, now: number): Promise<Member | null> {
  const row = await db
    .prepare(
      `SELECT m.id, m.email, m.identity_status
         FROM auth_sessions s JOIN members m ON m.id = s.member_id
        WHERE s.token_hash = ? AND s.revoked_at IS NULL AND s.expires_at > ?`,
    )
    .bind(await sha256Hex(token), now)
    .first<{ id: string; email: string; identity_status: string }>();
  return row ? { id: row.id, email: row.email, identityStatus: row.identity_status } : null;
}

/** Revokes one session token. The member's other sessions stay signed in. */
export async function revokeSession(db: D1Database, token: string, now: number): Promise<void> {
  await db
    .prepare(`UPDATE auth_sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`)
    .bind(now, await sha256Hex(token))
    .run();
}
