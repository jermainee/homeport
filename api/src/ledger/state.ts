import { type Balances, memberAccount } from "./book";

/** What a balance decision reads about a member, in one consistent snapshot. */
export interface MemberLedger {
  /** The `ledger_version` a guarded batch must still find. */
  version: number;
  balances: Balances;
  /** Credits open, unexpired reservations still hold. */
  reserved: number;
}

interface MemberLedgerRow {
  version: number;
  purchased: number;
  earned_pending: number;
  earned_cashable: number;
  reserved: number;
}

/** One statement reading a member's ledger version, balances and reserved credits. */
export function memberLedgerStatement(db: D1Database, memberId: string, now: number): D1PreparedStatement {
  return db
    .prepare(
      `SELECT m.ledger_version AS version,
              p.balance AS purchased, ep.balance AS earned_pending, ec.balance AS earned_cashable,
              (SELECT COALESCE(SUM(MAX(r.credits - r.charged_credits, 0)), 0)
                 FROM credit_reservations r
                WHERE r.member_id = m.id AND r.released_at IS NULL AND r.expires_at > ?) AS reserved
         FROM members m
         JOIN ledger_accounts p ON p.id = ?
         JOIN ledger_accounts ep ON ep.id = ?
         JOIN ledger_accounts ec ON ec.id = ?
        WHERE m.id = ?`,
    )
    .bind(
      now,
      memberAccount(memberId, "purchased"),
      memberAccount(memberId, "earned_pending"),
      memberAccount(memberId, "earned_cashable"),
      memberId,
    );
}

/** The member's ledger from a `memberLedgerStatement` result; throws if there is no such member. */
export function memberLedgerFrom(result: D1Result | undefined): MemberLedger {
  const row = result?.results[0] as MemberLedgerRow | undefined;
  if (!row) throw new Error("unknown member");
  return {
    version: row.version,
    balances: { purchased: row.purchased, earned_pending: row.earned_pending, earned_cashable: row.earned_cashable },
    reserved: row.reserved,
  };
}
