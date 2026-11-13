import { every } from "hono/combine";
import { createMiddleware } from "hono/factory";
import { requireSession, type SessionVariables } from "./auth/middleware";
import { sha256Hex } from "./crypto";
import { UUID_V7, uuidV7Timestamp } from "./formats";
import type { GatewayVariables } from "./gateway/auth";
import { apiError, invalidRequest, readJsonObject } from "./http";
import { requireVerifiedIdentity } from "./identity/require-verified";

const HOUR_MS = 60 * 60 * 1000;
/** A request_id older than this is rejected as expired. */
export const REQUEST_ID_MAX_AGE_MS = 24 * HOUR_MS;
/** A request_id further ahead than this is rejected as invalid (clock skew allowance). */
export const REQUEST_ID_MAX_SKEW_MS = HOUR_MS;
/** How long a record is kept after first sight; covers the whole accepted window. */
export const RECORD_TTL_MS = 25 * HOUR_MS;
/**
 * A claim without a stored response after this long belongs to a request that
 * died (evicted isolate, cancelled request), so a retry takes it over.
 */
export const CLAIM_TIMEOUT_MS = 60 * 1000;


export interface IdempotentVariables {
  /** The parsed JSON body, for the handler behind `idempotent`. */
  body: Record<string, unknown>;
}

/**
 * Makes a JSON POST safe to retry, as the contract requires: the body's
 * `request_id` (UUIDv7) is claimed once, the first response is stored, and a
 * retry with the same body gets it again. The same request_id with another
 * body, on another endpoint, or from another member or gateway, gets 409:
 * behind requireSession or requireGateway the caller is part of the
 * request, so no caller ever sees another's response (use memberIdempotent
 * for members, which fixes that order). 5xx responses are not kept, so a
 * retry runs again.
 */
export const idempotent = createMiddleware<{
  Bindings: Env;
  Variables: IdempotentVariables & Partial<SessionVariables> & Partial<GatewayVariables>;
}>(
  async (c, next) => {
    const body = await readJsonObject(c.req.raw);
    if (!body) return invalidRequest(c, "body must be a JSON object");
    const requestId = body["request_id"];
    if (typeof requestId !== "string" || !UUID_V7.test(requestId)) {
      return invalidRequest(c, "request_id must be a lowercase UUIDv7");
    }
    const now = Date.now();
    const issuedAt = uuidV7Timestamp(requestId);
    if (issuedAt < now - REQUEST_ID_MAX_AGE_MS) {
      return apiError(c, 400, "request_id_expired", "request_id is older than 24 hours");
    }
    if (issuedAt > now + REQUEST_ID_MAX_SKEW_MS) {
      return invalidRequest(c, "request_id is more than 1 hour in the future");
    }

    const db = c.env.DB;
    const fingerprint = await requestFingerprint(
      c.req.method,
      new URL(c.req.url).pathname,
      body,
      c.get("member")?.id,
      c.get("gateway")?.id,
    );
    const claim = await db
      .prepare(
        `INSERT INTO idempotency_records (request_id, fingerprint, claimed_at, created_at, expires_at)
         VALUES (?, ?, ?, ?, ?) ON CONFLICT (request_id) DO NOTHING`,
      )
      .bind(requestId, fingerprint, now, now, now + RECORD_TTL_MS)
      .run();

    if (claim.meta.changes !== 1) {
      const record = await db
        .prepare(
          `SELECT fingerprint, claimed_at, response_status, response_body
             FROM idempotency_records WHERE request_id = ?`,
        )
        .bind(requestId)
        .first<{
          fingerprint: string;
          claimed_at: number;
          response_status: number | null;
          response_body: string | null;
        }>();
      if (!record || record.fingerprint !== fingerprint) {
        return apiError(c, 409, "idempotency_conflict", "request_id was used with another request");
      }
      if (record.response_status !== null && record.response_body !== null) {
        return new Response(record.response_body, {
          status: record.response_status,
          headers: { "content-type": "application/json" },
        });
      }
      if (!(await takeOverAbandonedClaim(db, requestId, record.claimed_at, now))) {
        c.header("retry-after", "1");
        return apiError(c, 429, "rate_limited", "the first request with this request_id is still running");
      }
    }

    c.set("body", body);
    await next();

    if (c.res.status >= 500) {
      await db.prepare(`DELETE FROM idempotency_records WHERE request_id = ?`).bind(requestId).run();
      return;
    }
    await db
      .prepare(
        `UPDATE idempotency_records SET response_status = ?, response_body = ? WHERE request_id = ?`,
      )
      .bind(c.res.status, await c.res.clone().text(), requestId)
      .run();
  },
);

/**
 * requireSession, then idempotent: the order that makes the member part of
 * the request. Use this on every member endpoint that takes a request_id;
 * `idempotent` alone is for endpoints without a session.
 */
export const memberIdempotent = every(requireSession, idempotent);

/**
 * requireSession, requireVerifiedIdentity, then idempotent: for member
 * endpoints behind the identity gate. The identity check runs before
 * idempotent, so a 403 is not stored for the request_id.
 */
export const verifiedMemberIdempotent = every(requireSession, requireVerifiedIdentity, idempotent);

/**
 * SHA-256 of method, path, the caller (signed-in member or gateway, if any)
 * and canonical body: what "the same request" means.
 */
export function requestFingerprint(
  method: string,
  path: string,
  body: Record<string, unknown>,
  memberId?: string,
  gatewayId?: string,
): Promise<string> {
  const caller = memberId !== undefined ? ` member:${memberId}` : gatewayId !== undefined ? ` gateway:${gatewayId}` : "";
  return sha256Hex(`${method} ${path}${caller}\n${canonicalJson(body)}`);
}

/**
 * Claims a record whose request went silent for CLAIM_TIMEOUT_MS. The guarded
 * UPDATE lets only one of several concurrent retries win.
 */
async function takeOverAbandonedClaim(
  db: D1Database,
  requestId: string,
  claimedAt: number,
  now: number,
): Promise<boolean> {
  if (claimedAt > now - CLAIM_TIMEOUT_MS) return false;
  const taken = await db
    .prepare(
      `UPDATE idempotency_records SET claimed_at = ?
        WHERE request_id = ? AND response_status IS NULL AND claimed_at = ?`,
    )
    .bind(now, requestId, claimedAt)
    .run();
  return taken.meta.changes === 1;
}


/** JSON with object keys sorted, so the same body always has the same fingerprint. */
function canonicalJson(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (typeof value === "object" && value !== null) {
    const entries = Object.entries(value as Record<string, unknown>)
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .map(([key, item]) => `${JSON.stringify(key)}:${canonicalJson(item)}`);
    return `{${entries.join(",")}}`;
  }
  return JSON.stringify(value);
}
