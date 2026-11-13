import {
  type Balances,
  type MemberAccountKind,
  bookStatements,
  memberAccount,
  platformAccount,
} from "./book";
import { CONFLICT, guardedBatch, retryOnConflict } from "./guarded-batch";

export type { Balances, MemberAccountKind } from "./book";
export { LedgerBusyError } from "./guarded-batch";
export { releaseReservation, reserveCredits, type ReservationResult } from "./reservations";
export { chargeUsage, type UsageInput, type UsageResult } from "./usage";

export type BookingResult =
  | { status: "booked"; transactionId: string }
  | { status: "duplicate"; transactionId: string };

/** The member's credits per account, or null if there is no such member. */
export async function getBalances(db: D1Database, memberId: string): Promise<Balances | null> {
  const row = await db
    .prepare(
      `SELECT p.balance AS purchased, ep.balance AS earned_pending, ec.balance AS earned_cashable
         FROM ledger_accounts p
         JOIN ledger_accounts ep ON ep.id = ?
         JOIN ledger_accounts ec ON ec.id = ?
        WHERE p.id = ?`,
    )
    .bind(
      memberAccount(memberId, "earned_pending"),
      memberAccount(memberId, "earned_cashable"),
      memberAccount(memberId, "purchased"),
    )
    .first<Balances>();
  return row ? { purchased: row.purchased, earned_pending: row.earned_pending, earned_cashable: row.earned_cashable } : null;
}

/**
 * Gives a member credits from a platform source: `credits_issued` for sold
 * credit packs, `dev_grants` for development. They land in `purchased`
 * unless another account is named.
 */
export async function grantCredits(
  db: D1Database,
  input: {
    memberId: string;
    credits: number;
    source: "credits_issued" | "dev_grants";
    account?: MemberAccountKind;
    idempotencyKey: string;
    now?: number;
  },
): Promise<BookingResult> {
  if (!Number.isSafeInteger(input.credits) || input.credits <= 0) {
    throw new RangeError("credits must be a positive whole number");
  }
  const now = input.now ?? Date.now();
  const kind = input.source === "dev_grants" ? "dev_grant" : "purchase";

  return retryOnConflict(async () => {
    const [member, existing] = await db.batch<{ ledger_version?: number; id?: string }>([
      db.prepare(`SELECT ledger_version FROM members WHERE id = ?`).bind(input.memberId),
      db.prepare(`SELECT id FROM ledger_transactions WHERE idempotency_key = ?`).bind(input.idempotencyKey),
    ]);
    const duplicateId = existing?.results[0]?.id;
    if (duplicateId !== undefined) return { status: "duplicate", transactionId: duplicateId } as const;
    const version = member?.results[0]?.ledger_version;
    if (version === undefined) throw new Error("unknown member");

    const transactionId = crypto.randomUUID();
    const written = await guardedBatch(db, input.memberId, version, (guard) =>
      bookStatements(db, guard, {
        id: transactionId,
        idempotencyKey: input.idempotencyKey,
        kind,
        memberId: input.memberId,
        now,
        entries: [
          { accountId: platformAccount(input.source), amount: -input.credits },
          { accountId: memberAccount(input.memberId, input.account ?? "purchased"), amount: input.credits },
        ],
      }),
    );
    return written === CONFLICT ? CONFLICT : ({ status: "booked", transactionId } as const);
  });
}
