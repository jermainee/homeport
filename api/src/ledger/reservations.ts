import { CONFLICT, guardedBatch, retryOnConflict } from "./guarded-batch";
import { availableCredits } from "./spend";
import { memberLedgerFrom, memberLedgerStatement } from "./state";

export type ReservationResult = { status: "reserved" } | { status: "insufficient"; availableCredits: number };

/**
 * Holds `credits` back from the member's available credits, for a quota
 * lease. Nothing is booked: usage booked against the reservation reduces
 * what it holds, and releasing it frees the rest. Refused when the member's
 * available credits (balance minus open reservations) do not cover it.
 *
 * The check and the insert run behind the member's guard, so concurrent
 * reservations never commit more than the member has. Reserving an id that
 * the member already reserved is answered `reserved` again and holds
 * nothing more.
 */
export async function reserveCredits(
  db: D1Database,
  input: { reservationId: string; memberId: string; credits: number; expiresAt: number; now?: number },
): Promise<ReservationResult> {
  if (!Number.isSafeInteger(input.credits) || input.credits <= 0) {
    throw new RangeError("credits must be a positive whole number");
  }
  const now = input.now ?? Date.now();
  // A reservation that is already over would hold nothing; an expiry in
  // seconds instead of milliseconds is the likely cause.
  if (!Number.isSafeInteger(input.expiresAt) || input.expiresAt <= now) {
    throw new RangeError("expiresAt must be a later time than now, in epoch milliseconds");
  }

  return retryOnConflict(async () => {
    const [memberResult, existingResult] = await db.batch([
      memberLedgerStatement(db, input.memberId, now),
      db.prepare(`SELECT member_id FROM credit_reservations WHERE id = ?`).bind(input.reservationId),
    ]);
    const existing = existingResult?.results[0] as { member_id: string } | undefined;
    if (existing) {
      if (existing.member_id !== input.memberId) throw new Error("the reservation id belongs to another member");
      return { status: "reserved" } as const;
    }
    const member = memberLedgerFrom(memberResult);
    const available = availableCredits(member.balances, member.reserved);
    if (available < input.credits) return { status: "insufficient", availableCredits: Math.max(available, 0) } as const;

    const written = await guardedBatch(db, [{ memberId: input.memberId, version: member.version }], (guard) => [
      db
        .prepare(
          `INSERT INTO credit_reservations (id, member_id, credits, created_at, expires_at)
           SELECT ?, ?, ?, ?, ? WHERE ${guard.sql}`,
        )
        .bind(input.reservationId, input.memberId, input.credits, now, input.expiresAt, ...guard.binds),
    ]);
    return written === CONFLICT ? CONFLICT : ({ status: "reserved" } as const);
  });
}

/**
 * Ends a reservation and returns the credits it still held: what usage did
 * not charge against it. A reservation that is unknown, already released or
 * expired releases nothing. Releasing only adds to the member's available
 * credits, so it needs no guard.
 */
export async function releaseReservation(
  db: D1Database,
  input: { reservationId: string; now?: number },
): Promise<{ releasedCredits: number }> {
  const now = input.now ?? Date.now();
  const released = await db
    .prepare(
      `UPDATE credit_reservations SET released_at = ?
        WHERE id = ? AND released_at IS NULL
       RETURNING credits, charged_credits, expires_at`,
    )
    .bind(now, input.reservationId)
    .first<{ credits: number; charged_credits: number; expires_at: number }>();
  if (!released || released.expires_at <= now) return { releasedCredits: 0 };
  return { releasedCredits: Math.max(released.credits - released.charged_credits, 0) };
}
