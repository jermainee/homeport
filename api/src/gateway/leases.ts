import type { Context } from "hono";
import { ID, UUID_V7, timestamp } from "../formats";
import { apiError, hasOnlyFields, invalidRequest } from "../http";
import type { IdempotentVariables } from "../idempotency";
import { releaseReservation, reserveCredits } from "../ledger";
import { LEASE_CREDITS, LEASE_GRACE_SECONDS, LEASE_TTL_SECONDS, leaseTerms } from "../pricing";
import type { GatewayVariables } from "./auth";
import { PEER_JOINS, SERVED_PEER_SQL } from "./served";

type GatewayContext = Context<{ Bindings: Env; Variables: GatewayVariables & IdempotentVariables }>;

interface LeaseRow {
  id: string;
  peer_id: string;
  session_id: string;
  granted_bytes: number;
  renew_at_bytes: number;
  grace_seconds: number;
  expires_at: number;
}

/** A lease as the contract's Lease schema writes it. */
function leaseBody(lease: LeaseRow) {
  return {
    lease_id: lease.id,
    granted_bytes: lease.granted_bytes,
    renew_at_bytes: lease.renew_at_bytes,
    expires_at: timestamp(lease.expires_at),
    grace_seconds: lease.grace_seconds,
  };
}

const LEASE_COLUMNS = `id, peer_id, session_id, granted_bytes, renew_at_bytes, grace_seconds, expires_at`;

/**
 * POST /v1/gateway/leases (contract createLease). Reserves credits for a
 * peer's session and grants the bytes they buy; the gateway never sees a
 * price.
 *
 * - The peer must be one this gateway serves (see SERVED_PEER_SQL), or 403.
 * - A renewal names the session's current lease. A lease is superseded
 *   once: renewing it again returns its successor with 200.
 * - A member with fewer than LEASE_CREDITS available gets a smaller last
 *   lease; one with none gets 402 and the grace period.
 * - The reservation and the lease row commit in one guarded batch.
 */
export async function createLease(c: GatewayContext): Promise<Response> {
  const body = c.get("body");
  if (!hasOnlyFields(body, ["request_id", "peer_id", "session_id", "supersedes_lease_id"])) {
    return invalidRequest(c, "unknown field");
  }
  const peerId = body["peer_id"];
  const sessionId = body["session_id"];
  const supersedes = body["supersedes_lease_id"];
  if (typeof peerId !== "string" || !ID.test(peerId)) return invalidRequest(c, "peer_id must be an id");
  if (typeof sessionId !== "string" || !UUID_V7.test(sessionId)) return invalidRequest(c, "session_id must be a UUIDv7");
  if (supersedes !== undefined && (typeof supersedes !== "string" || !ID.test(supersedes))) {
    return invalidRequest(c, "supersedes_lease_id must be an id");
  }

  const db = c.env.DB;
  const gatewayId = c.get("gateway").id;
  const peer = await db
    .prepare(
      `SELECT p.member_id, p.line_id FROM ${PEER_JOINS}
        WHERE p.id = ? AND l.gateway_id = ? AND ${SERVED_PEER_SQL}`,
    )
    .bind(peerId, gatewayId)
    .first<{ member_id: string; line_id: string }>();
  if (!peer) return c.json({ error: "peer_inactive" }, 403);

  if (supersedes !== undefined) {
    const [currentResult, successorResult] = await db.batch([
      db.prepare(`SELECT ${LEASE_COLUMNS} FROM leases WHERE id = ? AND gateway_id = ?`).bind(supersedes, gatewayId),
      db.prepare(`SELECT ${LEASE_COLUMNS} FROM leases WHERE supersedes_lease_id = ?`).bind(supersedes),
    ]);
    const current = currentResult?.results[0] as LeaseRow | undefined;
    if (!current) return apiError(c, 404, "not_found");
    if (current.peer_id !== peerId || current.session_id !== sessionId) {
      return invalidRequest(c, "supersedes_lease_id belongs to another session");
    }
    const successor = successorResult?.results[0] as LeaseRow | undefined;
    if (successor) return c.json(leaseBody(successor), 200);
  }

  const now = Date.now();
  const leaseId = crypto.randomUUID();
  // Whole seconds, as the contract writes timestamps.
  const expiresAt = Math.floor((now + LEASE_TTL_SECONDS * 1000) / 1000) * 1000;
  const reserved = await reserveCredits(db, {
    reservationId: leaseId,
    memberId: peer.member_id,
    credits: LEASE_CREDITS,
    minimumCredits: 1,
    expiresAt,
    now,
    alsoWrite: (guard, credits) => {
      const terms = leaseTerms(credits);
      return [
        db
          .prepare(
            `INSERT INTO leases (id, gateway_id, peer_id, member_id, line_id, session_id, credits, granted_bytes,
                                 renew_at_bytes, grace_seconds, expires_at, supersedes_lease_id, created_at)
             SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE ${guard.sql}`,
          )
          .bind(
            leaseId,
            gatewayId,
            peerId,
            peer.member_id,
            peer.line_id,
            sessionId,
            terms.credits,
            terms.grantedBytes,
            terms.renewAtBytes,
            LEASE_GRACE_SECONDS,
            expiresAt,
            supersedes ?? null,
            now,
            ...guard.binds,
          ),
      ];
    },
  });
  if (reserved.status === "insufficient") {
    return c.json({ error: "insufficient_credits", grace_seconds: LEASE_GRACE_SECONDS }, 402);
  }
  const terms = leaseTerms(reserved.credits);
  return c.json(
    leaseBody({
      id: leaseId,
      peer_id: peerId,
      session_id: sessionId,
      granted_bytes: terms.grantedBytes,
      renew_at_bytes: terms.renewAtBytes,
      grace_seconds: LEASE_GRACE_SECONDS,
      expires_at: expiresAt,
    }),
    201,
  );
}

/**
 * POST /v1/gateway/leases/{lease_id}/return (contract returnLease). Releases
 * the credits the lease still holds: what usage did not charge against it.
 * A lease returned before, or expired, releases nothing and still gets 200.
 */
export async function returnLease(c: GatewayContext): Promise<Response> {
  if (!hasOnlyFields(c.get("body"), ["request_id"])) return invalidRequest(c, "unknown field");
  const leaseId = c.req.param("lease_id") ?? "";
  if (!ID.test(leaseId)) return invalidRequest(c, "lease_id must be an id");

  const db = c.env.DB;
  const lease = await db
    .prepare(`SELECT id FROM leases WHERE id = ? AND gateway_id = ?`)
    .bind(leaseId, c.get("gateway").id)
    .first();
  if (!lease) return apiError(c, 404, "not_found");

  const now = Date.now();
  const { releasedCredits } = await releaseReservation(db, { reservationId: leaseId, now });
  await db.prepare(`UPDATE leases SET returned_at = ? WHERE id = ? AND returned_at IS NULL`).bind(now, leaseId).run();
  return c.json({ released_credits: releasedCredits }, 200);
}
