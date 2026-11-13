import { applyD1Migrations } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";

describe("migration 0002 (credit ledger)", () => {
  it("gives members who signed up before it their three ledger accounts", async () => {
    const db = env.MIGRATION_TEST_DB;
    const before = env.TEST_MIGRATIONS.filter((migration) => migration.name < "0002");
    await applyD1Migrations(db, before);
    const memberId = crypto.randomUUID();
    await db
      .prepare(`INSERT INTO members (id, email, created_at) VALUES (?, ?, ?)`)
      .bind(memberId, `early-${memberId}@example.com`, 1_700_000_000_000)
      .run();

    await applyD1Migrations(db, env.TEST_MIGRATIONS.filter((migration) => migration.name <= "0002_~"));

    const { results } = await db
      .prepare(`SELECT kind, balance, created_at FROM ledger_accounts WHERE member_id = ? ORDER BY kind`)
      .bind(memberId)
      .all();
    expect(results).toEqual([
      { kind: "earned_cashable", balance: 0, created_at: 1_700_000_000_000 },
      { kind: "earned_pending", balance: 0, created_at: 1_700_000_000_000 },
      { kind: "purchased", balance: 0, created_at: 1_700_000_000_000 },
    ]);
  });
});
