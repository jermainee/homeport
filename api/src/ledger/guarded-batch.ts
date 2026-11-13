/**
 * Serializes balance changes per member without interactive transactions.
 *
 * D1 runs a `db.batch` as one SQLite transaction but cannot hold one open
 * across a read and a later write. So a change:
 *
 * 1. reads the member's `ledger_version` together with whatever it decides
 *    on (balances, reservations), in one statement or one read batch;
 * 2. runs one write batch. Its first statement bumps `ledger_version` and
 *    sets `ledger_nonce` to a fresh nonce, but only if the version is still
 *    the one it read. Every later statement carries `guard.sql`, so it does
 *    nothing unless `ledger_nonce` still holds that nonce;
 * 3. if the first statement changed no row, another change won the race and
 *    nothing was written: it reads again and retries.
 *
 * A batch claims every member whose balance it changes, the members it
 * credits included: a member's spend splits its debit by the balances it
 * read, so a credit that lands in between must make that spend retry.
 * Every write that changes a member's balance therefore commits only if no
 * other such write committed since its read.
 */

/** A member's `ledger_version` as a change read it. */
export interface MemberVersion {
  memberId: string;
  version: number;
}

/** A batch's claim on its members' ledgers, for guarding its statements. */
export interface Guard {
  /** SQL condition, true only while this batch holds every claimed member's ledger. */
  readonly sql: string;
  /** Values for the `?` placeholders in `sql`, in order. */
  readonly binds: readonly string[];
}

/** Returned by an attempt that lost the race; `retryOnConflict` runs it again. */
export const CONFLICT: unique symbol = Symbol("ledger conflict");

/** Attempts per change before giving up. Each round lets at least one writer through. */
export const MAX_ATTEMPTS = 25;

/** Raised when a change keeps losing races, for example under a burst of writes for one member. */
export class LedgerBusyError extends Error {
  override readonly name = "LedgerBusyError";
}

/**
 * Runs `build`'s statements as one batch behind the members' version
 * checks. Returns their results, or CONFLICT if any member's version moved
 * since it was read and nothing was written.
 */
export async function guardedBatch(
  db: D1Database,
  members: readonly MemberVersion[],
  build: (guard: Guard) => D1PreparedStatement[],
): Promise<D1Result[] | typeof CONFLICT> {
  if (members.length === 0) throw new Error("a guarded batch claims at least one member");
  if (new Set(members.map((member) => member.memberId)).size !== members.length) {
    throw new Error("a guarded batch cannot claim a member twice");
  }
  const nonce = crypto.randomUUID();
  const claim = db
    .prepare(
      `UPDATE members SET ledger_version = ledger_version + 1, ledger_nonce = ?
        WHERE ${members.map(() => "(id = ? AND ledger_version = ?)").join(" OR ")}`,
    )
    .bind(nonce, ...members.flatMap((member) => [member.memberId, member.version]));
  const guard: Guard = {
    sql: members.map(() => "EXISTS (SELECT 1 FROM members WHERE id = ? AND ledger_nonce = ?)").join(" AND "),
    binds: members.flatMap((member) => [member.memberId, nonce]),
  };
  // If only some members were claimed, the guard fails for every statement,
  // so nothing is written; the claimed versions just moved on.
  const [claimed, ...results] = await db.batch([claim, ...build(guard)]);
  return claimed?.meta.changes === members.length ? results : CONFLICT;
}

/** Runs `attempt` until it does not return CONFLICT, with a short random backoff between tries. */
export async function retryOnConflict<T>(attempt: () => Promise<T | typeof CONFLICT>): Promise<T> {
  for (let tries = 1; tries <= MAX_ATTEMPTS; tries++) {
    const result = await attempt();
    if (result !== CONFLICT) return result;
    await sleep(Math.random() * Math.min(2 ** tries, 50));
  }
  throw new LedgerBusyError(`gave up after ${MAX_ATTEMPTS} attempts`);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
