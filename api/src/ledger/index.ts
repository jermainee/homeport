import { type Balances, type MemberAccountKind, bookStatements, memberAccount, platformAccount } from "./book";
import { CONFLICT, guardedBatch, retryOnConflict } from "./guarded-batch";
import { memberLedgerFrom, memberLedgerStatement } from "./state";

export type { Balances, MemberAccountKind } from "./book";
export { type Guard, LedgerBusyError } from "./guarded-batch";
export { releaseReservation, reserveCredits, type ReservationResult } from "./reservations";
export { chargeUsage, type UsageInput, type UsageResult } from "./usage";

export type BookingResult =
  | { status: "booked"; transactionId: string }
  | { status: "duplicate"; transactionId: string };

/** The member's credits per account, or null if there is no such member. */
export async function getBalances(db: D1Database, memberId: string): Promise<Balances | null> {
  const result = await memberLedgerStatement(db, memberId, Date.now()).all();
  return result.results.length === 0 ? null : memberLedgerFrom(result).balances;
}

/** Where granted credits come from, and which of the member's accounts they may land in. */
export type GrantSource =
  /** Sold credit packs. Sold credits are never paid out, so they land in purchased only. */
  | { source: "credits_issued"; account?: "purchased" }
  /** Development and tests. */
  | { source: "dev_grants"; account?: MemberAccountKind };

/**
 * Gives a member credits from a platform source: `credits_issued` for sold
 * credit packs, `dev_grants` for development. They land in `purchased`
 * unless a dev grant names another account.
 */
export async function grantCredits(
  db: D1Database,
  input: GrantSource & {
    memberId: string;
    credits: number;
    idempotencyKey: string;
    now?: number;
  },
): Promise<BookingResult> {
  if (!Number.isSafeInteger(input.credits) || input.credits <= 0) {
    throw new RangeError("credits must be a positive whole number");
  }
  const account: MemberAccountKind = input.account ?? "purchased";
  if (input.source === "credits_issued" && account !== "purchased") {
    // The type already says so; this guards callers that bypass it.
    throw new Error("sold credits land in purchased only");
  }
  const now = input.now ?? Date.now();
  const kind = input.source === "dev_grants" ? "dev_grant" : "purchase";

  return retryOnConflict(async () => {
    const [memberResult, existingResult] = await db.batch([
      memberLedgerStatement(db, input.memberId, now),
      db
        .prepare(`SELECT id, member_id, kind FROM ledger_transactions WHERE idempotency_key = ?`)
        .bind(input.idempotencyKey),
    ]);
    const existing = existingResult?.results[0] as { id: string; member_id: string; kind: string } | undefined;
    if (existing) {
      if (existing.member_id !== input.memberId || existing.kind !== kind) {
        throw new Error("the idempotency key already belongs to another booking");
      }
      return { status: "duplicate", transactionId: existing.id } as const;
    }
    const member = memberLedgerFrom(memberResult);

    const transactionId = crypto.randomUUID();
    const written = await guardedBatch(db, [{ memberId: input.memberId, version: member.version }], (guard) =>
      bookStatements(db, guard, {
        id: transactionId,
        idempotencyKey: input.idempotencyKey,
        kind,
        memberId: input.memberId,
        now,
        entries: [
          { accountId: platformAccount(input.source), amount: -input.credits },
          { accountId: memberAccount(input.memberId, account), amount: input.credits },
        ],
      }),
    );
    return written === CONFLICT ? CONFLICT : ({ status: "booked", transactionId } as const);
  });
}
