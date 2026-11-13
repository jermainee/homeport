import { SHARE_CREDITS_PER_MB, USE_CREDITS_PER_MB, creditsForBytes } from "../pricing";
import { type Entry, MEMBER_ACCOUNT_KINDS, bookStatements, memberAccount, platformAccount } from "./book";
import { CONFLICT, guardedBatch, retryOnConflict } from "./guarded-batch";
import { spendOrder } from "./spend";
import { memberLedgerFrom, memberLedgerStatement } from "./state";

export interface UsageInput {
  /** The running total this usage adds to, for example one WireGuard peer. */
  meterId: string;
  /** The member who used the line. */
  payerMemberId: string;
  /** The member who owns the line. */
  payeeMemberId: string;
  /** Bytes used, upload plus download. */
  bytes: number;
  /** Identifies this piece of usage, for example a usage event id; a retry with it is not counted again. */
  usageKey: string;
  /** The credit reservation (quota lease) this usage counts against, if any. */
  reservationId?: string;
  now?: number;
}

export type UsageResult =
  | { status: "applied"; chargedCredits: number; earnedCredits: number }
  | { status: "duplicate" };

interface MeterRow {
  payer_member_id: string;
  payee_member_id: string;
  total_bytes: number;
  charged_credits: number;
  earned_credits: number;
}

/**
 * Adds metered usage to its meter and books what it costs: the user pays at
 * the use rate, the line's owner earns at the share rate (into
 * earned_pending), and the platform keeps the difference.
 *
 * Both amounts are taken from the meter's running total
 * (floor(total × rate / 10^6) minus what the meter already charged or paid),
 * so rounding never adds up across pieces of usage. Usage that does not add
 * up to a whole credit yet is counted but books nothing.
 *
 * The user's credits are spent in order (see spendOrder). Usage is always
 * booked, even past the user's balance: the gateway is the meter of record,
 * and quota leases keep the overrun to at most one lease.
 */
export async function chargeUsage(db: D1Database, input: UsageInput): Promise<UsageResult> {
  const { meterId, payerMemberId, payeeMemberId, bytes, usageKey, reservationId } = input;
  if (payerMemberId === payeeMemberId) throw new Error("a member never pays for their own line");
  if (!Number.isSafeInteger(bytes) || bytes < 0) throw new RangeError("bytes must be a non-negative whole number");
  const now = input.now ?? Date.now();

  return retryOnConflict(async () => {
    const [payerResult, payeeResult, meterResult, appliedResult] = await db.batch([
      memberLedgerStatement(db, payerMemberId, now),
      db.prepare(`SELECT ledger_version FROM members WHERE id = ?`).bind(payeeMemberId),
      db
        .prepare(
          `SELECT payer_member_id, payee_member_id, total_bytes, charged_credits, earned_credits
             FROM meter_totals WHERE meter_id = ?`,
        )
        .bind(meterId),
      db.prepare(`SELECT meter_id FROM usage_charges WHERE usage_key = ?`).bind(usageKey),
    ]);
    const applied = appliedResult?.results[0] as { meter_id: string } | undefined;
    if (applied) {
      if (applied.meter_id !== meterId) throw new Error("the usage key was already applied to another meter");
      return { status: "duplicate" } as const;
    }
    const payer = memberLedgerFrom(payerResult);
    const payeeVersion = (payeeResult?.results[0] as { ledger_version: number } | undefined)?.ledger_version;
    if (payeeVersion === undefined) throw new Error("unknown payee");
    const meter = meterResult?.results[0] as MeterRow | undefined;
    if (meter && (meter.payer_member_id !== payerMemberId || meter.payee_member_id !== payeeMemberId)) {
      throw new Error("a meter belongs to one payer and one payee");
    }

    const totalBytes = (meter?.total_bytes ?? 0) + bytes;
    const charged = creditsForBytes(totalBytes, USE_CREDITS_PER_MB) - (meter?.charged_credits ?? 0);
    const earned = creditsForBytes(totalBytes, SHARE_CREDITS_PER_MB) - (meter?.earned_credits ?? 0);
    const transactionId = charged > 0 ? crypto.randomUUID() : null;

    // The owner is claimed too: their earned_pending changes, and a spend of
    // theirs that read the old balance must retry.
    const members = [
      { memberId: payerMemberId, version: payer.version },
      { memberId: payeeMemberId, version: payeeVersion },
    ];
    const written = await guardedBatch(db, members, (guard) => {
      const taken = spendOrder(payer.balances, charged);
      const entries: Entry[] = [
        ...MEMBER_ACCOUNT_KINDS.map((kind) => ({ accountId: memberAccount(payerMemberId, kind), amount: -taken[kind] })),
        { accountId: memberAccount(payeeMemberId, "earned_pending"), amount: earned },
        { accountId: platformAccount("platform_revenue"), amount: charged - earned },
      ];
      return [
        db
          .prepare(
            `INSERT INTO meter_totals
               (meter_id, payer_member_id, payee_member_id, total_bytes, charged_credits, earned_credits, updated_at)
             SELECT ?, ?, ?, ?, ?, ?, ? WHERE ${guard.sql}
             ON CONFLICT (meter_id) DO UPDATE SET
               payer_member_id = excluded.payer_member_id,
               payee_member_id = excluded.payee_member_id,
               total_bytes = excluded.total_bytes,
               charged_credits = excluded.charged_credits,
               earned_credits = excluded.earned_credits,
               updated_at = excluded.updated_at`,
          )
          .bind(
            meterId,
            payerMemberId,
            payeeMemberId,
            totalBytes,
            (meter?.charged_credits ?? 0) + charged,
            (meter?.earned_credits ?? 0) + earned,
            now,
            ...guard.binds,
          ),
        ...(transactionId === null
          ? []
          : bookStatements(db, guard, {
              id: transactionId,
              idempotencyKey: `usage:${usageKey}`,
              kind: "usage",
              memberId: payerMemberId,
              entries,
              now,
            })),
        db
          .prepare(
            `INSERT INTO usage_charges (usage_key, meter_id, bytes, charged_credits, earned_credits, transaction_id, created_at)
             SELECT ?, ?, ?, ?, ?, ?, ? WHERE ${guard.sql}`,
          )
          .bind(usageKey, meterId, bytes, charged, earned, transactionId, now, ...guard.binds),
        ...(reservationId === undefined
          ? []
          : [
              db
                .prepare(
                  `UPDATE credit_reservations SET charged_credits = charged_credits + ?
                    WHERE id = ? AND member_id = ? AND ${guard.sql}`,
                )
                .bind(charged, reservationId, payerMemberId, ...guard.binds),
            ]),
      ];
    });
    if (written === CONFLICT) return CONFLICT;
    return { status: "applied", chargedCredits: charged, earnedCredits: earned } as const;
  });
}
