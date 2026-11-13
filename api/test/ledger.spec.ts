import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { chargeUsage, getBalances, grantCredits, releaseReservation, reserveCredits } from "../src/ledger";
import { CONFLICT, type Guard, guardedBatch } from "../src/ledger/guarded-batch";
import { memberLedgerFrom, memberLedgerStatement } from "../src/ledger/state";
import { LEASE_CREDITS } from "../src/pricing";
import { createMember, driftedBalances, platformBalance, unbalancedTransactions } from "./ledger-fixtures";

const db = env.DB;

describe("member accounts", () => {
  it("start at zero for a new member", async () => {
    const memberId = await createMember();

    expect(await getBalances(db, memberId)).toEqual({ purchased: 0, earned_pending: 0, earned_cashable: 0 });
  });

  it("do not exist for an unknown member", async () => {
    expect(await getBalances(db, crypto.randomUUID())).toBeNull();
  });
});

describe("grantCredits", () => {
  it("adds dev-granted credits to purchased, taken from the dev_grants account", async () => {
    const memberId = await createMember();
    const devGrantsBefore = await platformBalance("dev_grants");

    const result = await grantCredits(db, {
      memberId,
      credits: 5_000,
      source: "dev_grants",
      idempotencyKey: `dev_grant:${crypto.randomUUID()}`,
    });

    expect(result.status).toBe("booked");
    expect(await getBalances(db, memberId)).toEqual({ purchased: 5_000, earned_pending: 0, earned_cashable: 0 });
    expect(await platformBalance("dev_grants")).toBe(devGrantsBefore - 5_000);
  });

  it("books a retry with the same idempotency key only once", async () => {
    const memberId = await createMember();
    const grant = { memberId, credits: 700, source: "dev_grants" as const, idempotencyKey: `dev_grant:${crypto.randomUUID()}` };

    const first = await grantCredits(db, grant);
    const retry = await grantCredits(db, grant);

    expect(retry).toEqual({ status: "duplicate", transactionId: first.transactionId });
    expect((await getBalances(db, memberId))?.purchased).toBe(700);
  });

  it("refuses a grant that is not a positive whole number", async () => {
    const memberId = await createMember();
    for (const credits of [0, -5, 1.5]) {
      await expect(
        grantCredits(db, { memberId, credits, source: "dev_grants", idempotencyKey: crypto.randomUUID() }),
      ).rejects.toThrow(RangeError);
    }
  });

  it("keeps every transaction balanced and every cached balance equal to its entries", async () => {
    const memberId = await createMember();
    await grantCredits(db, { memberId, credits: 42, source: "dev_grants", idempotencyKey: crypto.randomUUID() });

    expect(await unbalancedTransactions()).toEqual([]);
    expect(await driftedBalances()).toEqual([]);
  });
});

describe("append-only ledger", () => {
  async function bookedGrant(): Promise<{ memberId: string; transactionId: string }> {
    const memberId = await createMember();
    const { transactionId } = await grantCredits(db, {
      memberId,
      credits: 100,
      source: "dev_grants",
      idempotencyKey: crypto.randomUUID(),
    });
    return { memberId, transactionId };
  }

  it("aborts every UPDATE and DELETE of entries", async () => {
    const { transactionId } = await bookedGrant();

    await expect(
      db.prepare(`UPDATE ledger_entries SET amount = amount * 2 WHERE transaction_id = ?`).bind(transactionId).run(),
    ).rejects.toThrow(/append-only/);
    await expect(
      db.prepare(`DELETE FROM ledger_entries WHERE transaction_id = ?`).bind(transactionId).run(),
    ).rejects.toThrow(/append-only/);
  });

  it("aborts every UPDATE and DELETE of transactions", async () => {
    const { transactionId } = await bookedGrant();

    await expect(
      db.prepare(`UPDATE ledger_transactions SET kind = 'other' WHERE id = ?`).bind(transactionId).run(),
    ).rejects.toThrow(/append-only/);
    await expect(db.prepare(`DELETE FROM ledger_transactions WHERE id = ?`).bind(transactionId).run()).rejects.toThrow(
      /append-only/,
    );
  });

  it("never lets an account change owner or kind, or disappear", async () => {
    const { memberId } = await bookedGrant();
    const account = `${memberId}:purchased`;

    await expect(
      db.prepare(`UPDATE ledger_accounts SET kind = 'earned_cashable' WHERE id = ?`).bind(account).run(),
    ).rejects.toThrow(/never change/);
    await expect(db.prepare(`DELETE FROM ledger_accounts WHERE id = ?`).bind(account).run()).rejects.toThrow(
      /never deleted/,
    );
  });

  it("rolls back a whole batch whose transaction does not sum to zero", async () => {
    const memberId = await createMember();
    const transactionId = crypto.randomUUID();
    const now = Date.now();

    await expect(
      db.batch([
        db
          .prepare(`INSERT INTO ledger_transactions (id, idempotency_key, kind, member_id, created_at) VALUES (?, ?, 'test', ?, ?)`)
          .bind(transactionId, crypto.randomUUID(), memberId, now),
        db
          .prepare(`INSERT INTO ledger_entries (transaction_id, account_id, amount, created_at) VALUES (?, 'platform:dev_grants', -10, ?)`)
          .bind(transactionId, now),
        db
          .prepare(`INSERT INTO ledger_entries (transaction_id, account_id, amount, created_at) VALUES (?, ?, 9, ?)`)
          .bind(transactionId, `${memberId}:purchased`, now),
        db.prepare(`INSERT INTO ledger_balance_checks (transaction_id) VALUES (?)`).bind(transactionId),
      ]),
    ).rejects.toThrow(/does not balance/);

    expect(await getBalances(db, memberId)).toEqual({ purchased: 0, earned_pending: 0, earned_cashable: 0 });
    const stored = await db.prepare(`SELECT id FROM ledger_transactions WHERE id = ?`).bind(transactionId).first();
    expect(stored).toBeNull();
  });

  it("rejects a balance check for a transaction with a single entry", async () => {
    const memberId = await createMember();
    const transactionId = crypto.randomUUID();
    const now = Date.now();

    await expect(
      db.batch([
        db
          .prepare(`INSERT INTO ledger_transactions (id, idempotency_key, kind, member_id, created_at) VALUES (?, ?, 'test', ?, ?)`)
          .bind(transactionId, crypto.randomUUID(), memberId, now),
        db.prepare(`INSERT INTO ledger_balance_checks (transaction_id) VALUES (?)`).bind(transactionId),
      ]),
    ).rejects.toThrow(/does not balance/);
  });
});

describe("chargeUsage", () => {
  async function userAndOwner(purchased: number): Promise<{ user: string; owner: string }> {
    const [user, owner] = await Promise.all([createMember(), createMember()]);
    if (purchased > 0) {
      await grantCredits(db, { memberId: user, credits: purchased, source: "dev_grants", idempotencyKey: crypto.randomUUID() });
    }
    return { user, owner };
  }

  it("writes alsoWrite's statements in the charge's batch, or nothing at all", async () => {
    const { user, owner } = await userAndOwner(100);
    const meterId = crypto.randomUUID();
    const link = (from: string, to: string) => (guard: Guard) => [
      db
        .prepare(`INSERT INTO member_links (member_id, linked_member_id, reason, created_at) SELECT ?, ?, 'charged', 0 WHERE ${guard.sql}`)
        .bind(from, to, ...guard.binds),
    ];

    await chargeUsage(db, { meterId, payerMemberId: user, payeeMemberId: owner, bytes: 1_000_000, usageKey: crypto.randomUUID(), alsoWrite: link(user, owner) });
    const failed = chargeUsage(db, { meterId, payerMemberId: user, payeeMemberId: owner, bytes: 1_000_000, usageKey: crypto.randomUUID(), alsoWrite: link(user, user) });

    await expect(failed).rejects.toThrow();
    expect(await db.prepare(`SELECT reason FROM member_links WHERE member_id = ?`).bind(user).first()).toEqual({ reason: "charged" });
    expect((await getBalances(db, user))?.purchased).toBe(90);
  });

  it("moves 10 credits per MB from the user: 5 to the line's owner, 5 to the platform", async () => {
    // The plan's control-plane check: 123,456,789 bytes in five events.
    const { user, owner } = await userAndOwner(1_000_000);
    const meterId = crypto.randomUUID();
    const revenueBefore = await platformBalance("platform_revenue");

    for (const bytes of [40_000_000, 50_000_000, 33_000_000, 456_000, 789]) {
      await chargeUsage(db, { meterId, payerMemberId: user, payeeMemberId: owner, bytes, usageKey: crypto.randomUUID() });
    }

    expect(await getBalances(db, user)).toEqual({ purchased: 998_766, earned_pending: 0, earned_cashable: 0 });
    expect(await getBalances(db, owner)).toEqual({ purchased: 0, earned_pending: 617, earned_cashable: 0 });
    expect(await platformBalance("platform_revenue")).toBe(revenueBefore + 617);
    expect(await unbalancedTransactions()).toEqual([]);
  });

  it("reports what each piece of usage charged and earned", async () => {
    const { user, owner } = await userAndOwner(10_000);

    const result = await chargeUsage(db, {
      meterId: crypto.randomUUID(),
      payerMemberId: user,
      payeeMemberId: owner,
      bytes: 2_500_000,
      usageKey: crypto.randomUUID(),
    });

    expect(result).toEqual({ status: "applied", chargedCredits: 25, earnedCredits: 12 });
  });

  it("counts a retried piece of usage only once", async () => {
    const { user, owner } = await userAndOwner(10_000);
    const usage = { meterId: crypto.randomUUID(), payerMemberId: user, payeeMemberId: owner, bytes: 3_000_000, usageKey: crypto.randomUUID() };

    await chargeUsage(db, usage);
    const retry = await chargeUsage(db, usage);

    expect(retry).toEqual({ status: "duplicate" });
    expect((await getBalances(db, user))?.purchased).toBe(10_000 - 30);
  });

  it("counts a retried piece of usage too small to charge yet only once", async () => {
    const { user, owner } = await userAndOwner(10_000);
    const meterId = crypto.randomUUID();
    const small = { meterId, payerMemberId: user, payeeMemberId: owner, bytes: 60_000, usageKey: crypto.randomUUID() };

    await chargeUsage(db, small);
    await chargeUsage(db, small);
    await chargeUsage(db, { ...small, usageKey: crypto.randomUUID() });

    // 120,000 bytes: 1 credit. Counting the retry would make it 180,000 bytes, still 1 credit,
    // so a third piece of 60,000 tells them apart: 2 credits only if the retry was counted.
    expect((await getBalances(db, user))?.purchased).toBe(10_000 - 1);
  });

  it("never charges a member for their own line", async () => {
    const { user } = await userAndOwner(10_000);

    await expect(
      chargeUsage(db, { meterId: crypto.randomUUID(), payerMemberId: user, payeeMemberId: user, bytes: 1_000_000, usageKey: crypto.randomUUID() }),
    ).rejects.toThrow(/own line/);
  });

  it("refuses to move a meter to another payer or owner", async () => {
    const { user, owner } = await userAndOwner(10_000);
    const other = await createMember();
    const meterId = crypto.randomUUID();
    await chargeUsage(db, { meterId, payerMemberId: user, payeeMemberId: owner, bytes: 1_000_000, usageKey: crypto.randomUUID() });

    await expect(
      chargeUsage(db, { meterId, payerMemberId: user, payeeMemberId: other, bytes: 1_000_000, usageKey: crypto.randomUUID() }),
    ).rejects.toThrow(/meter/);
  });
});

describe("spend order", () => {
  async function memberWith(balances: { purchased: number; earned_pending: number; earned_cashable: number }) {
    const memberId = await createMember();
    for (const account of ["purchased", "earned_pending", "earned_cashable"] as const) {
      if (balances[account] > 0) {
        await grantCredits(db, { memberId, credits: balances[account], source: "dev_grants", account, idempotencyKey: crypto.randomUUID() });
      }
    }
    return memberId;
  }

  async function use(user: string, owner: string, bytes: number) {
    return chargeUsage(db, { meterId: crypto.randomUUID(), payerMemberId: user, payeeMemberId: owner, bytes, usageKey: crypto.randomUUID() });
  }

  it("spends purchased credits first, then earned pending, then earned cashable", async () => {
    const user = await memberWith({ purchased: 1_000, earned_pending: 500, earned_cashable: 300 });
    const owner = await createMember();

    await use(user, owner, 160_000_000); // 1,600 credits

    expect(await getBalances(db, user)).toEqual({ purchased: 0, earned_pending: 0, earned_cashable: 200 });
  });

  it("leaves earned credits alone while purchased credits cover the charge", async () => {
    const user = await memberWith({ purchased: 1_000, earned_pending: 500, earned_cashable: 300 });
    const owner = await createMember();

    await use(user, owner, 50_000_000); // 500 credits

    expect(await getBalances(db, user)).toEqual({ purchased: 500, earned_pending: 500, earned_cashable: 300 });
  });

  it("books metered usage past the balance as a negative purchased balance", async () => {
    const user = await memberWith({ purchased: 100, earned_pending: 0, earned_cashable: 50 });
    const owner = await createMember();

    await use(user, owner, 30_000_000); // 300 credits

    expect(await getBalances(db, user)).toEqual({ purchased: -150, earned_pending: 0, earned_cashable: 0 });
    expect(await unbalancedTransactions()).toEqual([]);
  });
});

describe("rounding", () => {
  it("never drifts: many small pieces of usage cost what their total costs", async () => {
    const [user, owner] = await Promise.all([createMember(), createMember()]);
    await grantCredits(db, { memberId: user, credits: 10_000, source: "dev_grants", idempotencyKey: crypto.randomUUID() });
    const meterId = crypto.randomUUID();
    const revenueBefore = await platformBalance("platform_revenue");

    let charged = 0;
    let earned = 0;
    // 250 pieces of 123,457 bytes = 30,864,250 bytes: 308 credits charged, 154 earned.
    // Rounding each piece down on its own would charge 250 and pay the owner nothing.
    for (let piece = 0; piece < 250; piece++) {
      const result = await chargeUsage(db, { meterId, payerMemberId: user, payeeMemberId: owner, bytes: 123_457, usageKey: crypto.randomUUID() });
      if (result.status === "applied") {
        charged += result.chargedCredits;
        earned += result.earnedCredits;
      }
    }

    expect({ charged, earned }).toEqual({ charged: 308, earned: 154 });
    expect((await getBalances(db, user))?.purchased).toBe(10_000 - 308);
    expect((await getBalances(db, owner))?.earned_pending).toBe(154);
    expect(await platformBalance("platform_revenue")).toBe(revenueBefore + 154);
  });
});

describe("credit reservations (quota leases)", () => {
  const HOUR = 60 * 60 * 1000;

  async function memberWithCredits(credits: number): Promise<string> {
    const memberId = await createMember();
    await grantCredits(db, { memberId, credits, source: "dev_grants", idempotencyKey: crypto.randomUUID() });
    return memberId;
  }

  function reserve(memberId: string, credits = LEASE_CREDITS, expiresAt = Date.now() + HOUR) {
    return reserveCredits(db, { reservationId: crypto.randomUUID(), memberId, credits, expiresAt });
  }

  it("grants exactly 3 of 10 concurrent leases against a balance for 3", async () => {
    const memberId = await memberWithCredits(3 * LEASE_CREDITS + LEASE_CREDITS - 1);

    const results = await Promise.all(Array.from({ length: 10 }, () => reserve(memberId)));

    expect(results.filter((result) => result.status === "reserved")).toHaveLength(3);
    expect(results.filter((result) => result.status === "insufficient")).toHaveLength(7);
    const { count } = (await db
      .prepare(`SELECT COUNT(*) AS count FROM credit_reservations WHERE member_id = ?`)
      .bind(memberId)
      .first<{ count: number }>()) ?? { count: -1 };
    expect(count).toBe(3);
  });

  it("books nothing: the balance stays, only the available credits shrink", async () => {
    const memberId = await memberWithCredits(LEASE_CREDITS);

    expect((await reserve(memberId)).status).toBe("reserved");

    expect((await getBalances(db, memberId))?.purchased).toBe(LEASE_CREDITS);
    expect(await reserve(memberId, 1)).toEqual({ status: "insufficient", availableCredits: 0 });
  });

  it("holds back only what usage has not charged against it yet", async () => {
    const memberId = await memberWithCredits(3_000);
    const owner = await createMember();
    const reservationId = crypto.randomUUID();
    await reserveCredits(db, { reservationId, memberId, credits: 2_000, expiresAt: Date.now() + HOUR });

    await chargeUsage(db, {
      meterId: crypto.randomUUID(),
      payerMemberId: memberId,
      payeeMemberId: owner,
      bytes: 50_000_000, // 500 credits
      usageKey: crypto.randomUUID(),
      reservationId,
    });

    // Balance 2,500, of which the reservation still holds 1,500.
    expect(await reserve(memberId, 1_001)).toEqual({ status: "insufficient", availableCredits: 1_000 });
    expect((await reserve(memberId, 1_000)).status).toBe("reserved");
  });

  it("releases what usage did not charge, once", async () => {
    const memberId = await memberWithCredits(2_000);
    const owner = await createMember();
    const reservationId = crypto.randomUUID();
    await reserveCredits(db, { reservationId, memberId, credits: 2_000, expiresAt: Date.now() + HOUR });
    await chargeUsage(db, {
      meterId: crypto.randomUUID(),
      payerMemberId: memberId,
      payeeMemberId: owner,
      bytes: 15_400_000, // 154 credits
      usageKey: crypto.randomUUID(),
      reservationId,
    });

    expect(await releaseReservation(db, { reservationId })).toEqual({ releasedCredits: 1_846 });
    expect(await releaseReservation(db, { reservationId })).toEqual({ releasedCredits: 0 });
    expect((await reserve(memberId, 1_846)).status).toBe("reserved");
  });

  it("stops holding credits once it expires, and then releases nothing", async () => {
    const memberId = await memberWithCredits(2_000);
    const reservationId = crypto.randomUUID();
    const now = Date.now();
    await reserveCredits(db, { reservationId, memberId, credits: 2_000, expiresAt: now + 1_000, now });

    expect((await reserveCredits(db, { reservationId: crypto.randomUUID(), memberId, credits: 2_000, expiresAt: now + HOUR, now: now + 1_000 })).status).toBe("reserved");
    expect(await releaseReservation(db, { reservationId, now: now + 2_000 })).toEqual({ releasedCredits: 0 });
  });

  it("treats a repeated reservation id as the same reservation", async () => {
    const memberId = await memberWithCredits(3_000);
    const reservationId = crypto.randomUUID();
    const expiresAt = Date.now() + HOUR;

    expect((await reserveCredits(db, { reservationId, memberId, credits: 2_000, expiresAt })).status).toBe("reserved");
    expect((await reserveCredits(db, { reservationId, memberId, credits: 2_000, expiresAt })).status).toBe("reserved");

    expect(await reserve(memberId, 1_001)).toEqual({ status: "insufficient", availableCredits: 1_000 });
  });

  it("refuses a reservation id that belongs to another member", async () => {
    const [first, second] = await Promise.all([memberWithCredits(2_000), memberWithCredits(2_000)]);
    const reservationId = crypto.randomUUID();
    await reserveCredits(db, { reservationId, memberId: first, credits: 2_000, expiresAt: Date.now() + HOUR });

    await expect(
      reserveCredits(db, { reservationId, memberId: second, credits: 2_000, expiresAt: Date.now() + HOUR }),
    ).rejects.toThrow(/another member/);
  });

  it("refuses a reservation that has already expired", async () => {
    const memberId = await memberWithCredits(2_000);
    const now = Date.now();

    await expect(
      reserveCredits(db, { reservationId: crypto.randomUUID(), memberId, credits: 10, expiresAt: now, now }),
    ).rejects.toThrow(RangeError);
    // Seconds instead of milliseconds is the likely mistake.
    await expect(
      reserveCredits(db, { reservationId: crypto.randomUUID(), memberId, credits: 10, expiresAt: Math.floor(now / 1000) + 3600, now }),
    ).rejects.toThrow(RangeError);
  });

  it("releases nothing for an unknown reservation", async () => {
    expect(await releaseReservation(db, { reservationId: crypto.randomUUID() })).toEqual({ releasedCredits: 0 });
  });

  it("refuses a reservation that is not a positive whole number", async () => {
    const memberId = await memberWithCredits(10);

    await expect(reserve(memberId, 0)).rejects.toThrow(RangeError);
    await expect(reserve(memberId, 2.5)).rejects.toThrow(RangeError);
  });

  it("reserves what is available, down to minimumCredits, and says how much", async () => {
    const memberId = await memberWithCredits(500);
    const reservationId = crypto.randomUUID();

    const partial = await reserveCredits(db, { reservationId, memberId, credits: 2_000, minimumCredits: 1, expiresAt: Date.now() + HOUR });
    const again = await reserveCredits(db, { reservationId, memberId, credits: 2_000, minimumCredits: 1, expiresAt: Date.now() + HOUR });
    const none = await reserveCredits(db, { reservationId: crypto.randomUUID(), memberId, credits: 2_000, minimumCredits: 1, expiresAt: Date.now() + HOUR });

    expect(partial).toEqual({ status: "reserved", credits: 500 });
    expect(again).toEqual({ status: "reserved", credits: 500 });
    expect(none).toEqual({ status: "insufficient", availableCredits: 0 });
  });

  it("writes alsoWrite's statements in the reservation's batch, with the credits reserved, or nothing at all", async () => {
    const memberId = await memberWithCredits(700);
    const other = await memberWithCredits(10);
    const reservationId = crypto.randomUUID();
    const failingId = crypto.randomUUID();
    const link = (from: string, to: string) => (guard: Guard, credits: number) => [
      db
        .prepare(`INSERT INTO member_links (member_id, linked_member_id, reason, created_at) SELECT ?, ?, ?, 0 WHERE ${guard.sql}`)
        .bind(from, to, `reserved:${credits}`, ...guard.binds),
    ];

    await reserveCredits(db, { reservationId, memberId, credits: 2_000, minimumCredits: 1, expiresAt: Date.now() + HOUR, alsoWrite: link(memberId, other) });
    // A member never links to themselves (CHECK), so this batch aborts, reservation included.
    const failed = reserveCredits(db, { reservationId: failingId, memberId: other, credits: 1, expiresAt: Date.now() + HOUR, alsoWrite: link(other, other) });

    await expect(failed).rejects.toThrow();
    const linked = await db.prepare(`SELECT reason FROM member_links WHERE member_id = ? AND linked_member_id = ?`).bind(memberId, other).first();
    expect(linked).toEqual({ reason: "reserved:700" });
    expect(await db.prepare(`SELECT 1 FROM credit_reservations WHERE id = ?`).bind(failingId).first()).toBeNull();
  });
});

describe("concurrent usage", () => {
  it("serializes usage charges for one member: nothing is lost or counted twice", async () => {
    const [user, owner] = await Promise.all([createMember(), createMember()]);
    await grantCredits(db, { memberId: user, credits: 1_000, source: "dev_grants", idempotencyKey: crypto.randomUUID() });
    const meterId = crypto.randomUUID();

    await Promise.all(
      Array.from({ length: 10 }, () =>
        chargeUsage(db, { meterId, payerMemberId: user, payeeMemberId: owner, bytes: 1_234_567, usageKey: crypto.randomUUID() }),
      ),
    );

    // 12,345,670 bytes: 123 credits charged, 61 earned.
    expect((await getBalances(db, user))?.purchased).toBe(1_000 - 123);
    expect((await getBalances(db, owner))?.earned_pending).toBe(61);
    const meter = await db.prepare(`SELECT total_bytes FROM meter_totals WHERE meter_id = ?`).bind(meterId).first<{ total_bytes: number }>();
    expect(meter?.total_bytes).toBe(12_345_670);
    expect(await unbalancedTransactions()).toEqual([]);
    expect(await driftedBalances()).toEqual([]);
  });
});

describe("guarded batch", () => {
  it("makes a change retry when a booking credited its member after it read", async () => {
    // An owner's spend must not split its debit by balances that a user's
    // usage has changed since, so crediting a member moves their version too.
    const [user, owner] = await Promise.all([createMember(), createMember()]);
    await grantCredits(db, { memberId: user, credits: 1_000, source: "dev_grants", idempotencyKey: crypto.randomUUID() });
    const stale = memberLedgerFrom(await memberLedgerStatement(db, owner, Date.now()).all());

    await chargeUsage(db, { meterId: crypto.randomUUID(), payerMemberId: user, payeeMemberId: owner, bytes: 1_000_000, usageKey: crypto.randomUUID() });

    expect(await guardedBatch(db, [{ memberId: owner, version: stale.version }], () => [])).toBe(CONFLICT);
  });

  it("refuses to claim the same member twice", async () => {
    const memberId = await createMember();

    await expect(
      guardedBatch(db, [{ memberId, version: 0 }, { memberId, version: 0 }], () => []),
    ).rejects.toThrow(/twice/);
  });
});

describe("idempotency keys", () => {
  it("refuse a grant whose key already booked another member's grant", async () => {
    const [first, second] = await Promise.all([createMember(), createMember()]);
    const idempotencyKey = crypto.randomUUID();
    await grantCredits(db, { memberId: first, credits: 10, source: "dev_grants", idempotencyKey });

    await expect(grantCredits(db, { memberId: second, credits: 10, source: "dev_grants", idempotencyKey })).rejects.toThrow(
      /another booking/,
    );
  });

  it("refuse usage whose key was already applied to another meter", async () => {
    const [user, owner] = await Promise.all([createMember(), createMember()]);
    const usageKey = crypto.randomUUID();
    await chargeUsage(db, { meterId: crypto.randomUUID(), payerMemberId: user, payeeMemberId: owner, bytes: 1, usageKey });

    await expect(
      chargeUsage(db, { meterId: crypto.randomUUID(), payerMemberId: user, payeeMemberId: owner, bytes: 1, usageKey }),
    ).rejects.toThrow(/another meter/);
  });
});

describe("guard rails", () => {
  it("never lets sold credits land in an earned account", async () => {
    const memberId = await createMember();

    await expect(
      // @ts-expect-error The type allows only purchased for sold credits; this is a caller that bypasses it.
      grantCredits(db, { memberId, credits: 10, source: "credits_issued", account: "earned_cashable", idempotencyKey: crypto.randomUUID() }),
    ).rejects.toThrow(/purchased/);
  });

  it("never lets a meter change payer or payee", async () => {
    const [user, owner, other] = await Promise.all([createMember(), createMember(), createMember()]);
    const meterId = crypto.randomUUID();
    await chargeUsage(db, { meterId, payerMemberId: user, payeeMemberId: owner, bytes: 1, usageKey: crypto.randomUUID() });

    await expect(
      db.prepare(`UPDATE meter_totals SET payee_member_id = ? WHERE meter_id = ?`).bind(other, meterId).run(),
    ).rejects.toThrow(/meter/);
  });
});
