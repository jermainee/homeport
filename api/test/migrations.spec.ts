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

describe("migration 0010 (line canary)", () => {
  it("keeps the line events recorded before it", async () => {
    const db = env.MIGRATION_TEST_DB;
    await applyD1Migrations(db, env.TEST_MIGRATIONS.filter((migration) => migration.name < "0010"));
    const memberId = crypto.randomUUID();
    const lineId = crypto.randomUUID();
    const eventId = crypto.randomUUID();
    await db.batch([
      db.prepare(`INSERT INTO members (id, email, created_at) VALUES (?, ?, ?)`).bind(memberId, `m-${memberId}@example.com`, 1),
      db
        .prepare(`INSERT INTO devices (id, member_id, name, platform, agent_public_key, spki_sha256, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`)
        .bind(lineId, memberId, "Mac", "macos", `key-${lineId}`, `spki-${lineId}`, 1),
      db
        .prepare(
          `INSERT INTO gateways (id, wg_public_key, wg_endpoint, quic_endpoint, quic_server_name, quic_spki_sha256, version, updated_at)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
        )
        .bind(`gw-${lineId}`, "k", "gw:51820", "gw:443", "gw", "ab", "0.1.0", 1),
      db
        .prepare(`INSERT INTO lines (id, device_id, member_id, gateway_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`)
        .bind(lineId, lineId, memberId, `gw-${lineId}`, 1, 1),
      db
        .prepare(`INSERT INTO line_events (event_id, gateway_id, line_id, type, agent_version, at, received_at) VALUES (?, ?, ?, ?, ?, ?, ?)`)
        .bind(eventId, `gw-${lineId}`, lineId, "online", "0.1.0", 5, 6),
    ]);

    await applyD1Migrations(db, env.TEST_MIGRATIONS.filter((migration) => migration.name <= "0010_~"));

    const row = await db.prepare(`SELECT type, at, tamper_check AS "check" FROM line_events WHERE event_id = ?`).bind(eventId).first();
    expect(row).toEqual({ type: "online", at: 5, check: null });
  });
});

