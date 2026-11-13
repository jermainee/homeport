import type { Guard } from "./guarded-batch";

/** A member's accounts, in the order usage spends them. */
export const MEMBER_ACCOUNT_KINDS = ["purchased", "earned_pending", "earned_cashable"] as const;
export type MemberAccountKind = (typeof MEMBER_ACCOUNT_KINDS)[number];

export type PlatformAccountKind = "platform_revenue" | "credits_issued" | "dev_grants";

/** Credits per member account kind. */
export type Balances = Record<MemberAccountKind, number>;

export function memberAccount(memberId: string, kind: MemberAccountKind): string {
  return `${memberId}:${kind}`;
}

export function platformAccount(kind: PlatformAccountKind): string {
  return `platform:${kind}`;
}

export interface Entry {
  accountId: string;
  /** Signed whole credits: positive adds to the account, negative takes from it. */
  amount: number;
}

export interface Transaction {
  id: string;
  /** The business event this books; a second booking with the same key fails. */
  idempotencyKey: string;
  kind: string;
  /** The member whose guard the batch holds. */
  memberId: string;
  entries: readonly Entry[];
  now: number;
}

/**
 * The statements that book one transaction inside a guarded batch: the
 * transaction, its entries, and the balance check, which aborts the whole
 * batch if the entries do not sum to zero. Zero amounts are left out.
 */
export function bookStatements(db: D1Database, guard: Guard, transaction: Transaction): D1PreparedStatement[] {
  const entries = transaction.entries.filter((entry) => entry.amount !== 0);
  for (const entry of entries) {
    if (!Number.isSafeInteger(entry.amount)) throw new RangeError("ledger amounts are whole credits");
  }
  if (entries.length < 2 || entries.reduce((sum, entry) => sum + entry.amount, 0) !== 0) {
    throw new Error("a ledger transaction needs at least two entries that sum to zero");
  }

  const { id, idempotencyKey, kind, memberId, now } = transaction;
  return [
    db
      .prepare(
        `INSERT INTO ledger_transactions (id, idempotency_key, kind, member_id, created_at)
         SELECT ?, ?, ?, ?, ? WHERE ${guard.sql}`,
      )
      .bind(id, idempotencyKey, kind, memberId, now, ...guard.binds),
    ...entries.map((entry) =>
      db
        .prepare(
          `INSERT INTO ledger_entries (transaction_id, account_id, amount, created_at)
           SELECT ?, ?, ?, ? WHERE ${guard.sql}`,
        )
        .bind(id, entry.accountId, entry.amount, now, ...guard.binds),
    ),
    db
      .prepare(`INSERT INTO ledger_balance_checks (transaction_id) SELECT ? WHERE ${guard.sql}`)
      .bind(id, ...guard.binds),
  ];
}
