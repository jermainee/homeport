import { createExecutionContext, createScheduledController, waitOnExecutionContext } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { RAW_EVENT_RETENTION_MS } from "../src/prune";
import worker from "../src/index";
import { gatewayCall, grantedLease, reportUsage, servedPeer, usageEvent } from "./gateway-fixtures";
import { uuidv7 } from "./helpers";

async function count(sql: string, ...binds: unknown[]): Promise<number> {
  return (await env.DB.prepare(sql).bind(...binds).first<{ n: number }>())?.n ?? -1;
}

async function runCron(scheduledTime: number) {
  const ctx = createExecutionContext();
  worker.scheduled(createScheduledController({ scheduledTime, cron: "17 * * * *" }), env, ctx);
  await waitOnExecutionContext(ctx);
}

describe("scheduled cleanup", () => {
  it(`deletes raw usage and line events, and their usage charges, ${RAW_EVENT_RETENTION_MS / 86_400_000} days after they arrived`, async () => {
    const { gateway, lineId, peer } = await servedPeer();
    const lease = await grantedLease(gateway.token, peer.peer_id);
    const event = usageEvent({ sessionId: lease.sessionId, peerId: peer.peer_id, lineId, leaseId: lease.lease_id, seq: 0, bytesUp: 0, bytesDown: 2_000_000 });
    await reportUsage(gateway.token, [event]);
    await gatewayCall(gateway.token, "POST", "/v1/gateway/line-events", {
      events: [{ event_id: uuidv7(), line_id: lineId, type: "online", observed_ip: "203.0.113.47", agent_version: "0.1.0", at: new Date().toISOString() }],
    });
    const ledgerBefore = await count(`SELECT COUNT(*) AS n FROM ledger_entries`);

    await runCron(Date.now() + RAW_EVENT_RETENTION_MS - 60_000);
    const keptWhileRecent = await count(`SELECT COUNT(*) AS n FROM usage_events WHERE event_id = ?`, event.event_id);
    await runCron(Date.now() + RAW_EVENT_RETENTION_MS + 60_000);

    expect(keptWhileRecent).toBe(1);
    expect(await count(`SELECT COUNT(*) AS n FROM usage_events WHERE event_id = ?`, event.event_id)).toBe(0);
    expect(await count(`SELECT COUNT(*) AS n FROM usage_charges WHERE usage_key = ?`, event.event_id)).toBe(0);
    expect(await count(`SELECT COUNT(*) AS n FROM line_events WHERE line_id = ?`, lineId)).toBe(0);
    // The ledger, meter totals and the session log stay.
    expect(await count(`SELECT COUNT(*) AS n FROM ledger_entries`)).toBe(ledgerBefore);
    expect(await count(`SELECT COUNT(*) AS n FROM meter_totals WHERE meter_id = ?`, peer.peer_id)).toBe(1);
    expect(await count(`SELECT COUNT(*) AS n FROM usage_sessions WHERE session_id = ?`, lease.sessionId)).toBe(1);
  });

  it("deletes idempotency records once they expired", async () => {
    const { gateway, peer } = await servedPeer();
    const lease = await grantedLease(gateway.token, peer.peer_id);
    const before = await count(`SELECT COUNT(*) AS n FROM idempotency_records`);

    await runCron(Date.now() + 26 * 60 * 60 * 1000);

    expect(before).toBeGreaterThan(0);
    expect(await count(`SELECT COUNT(*) AS n FROM idempotency_records`)).toBe(0);
    expect(await count(`SELECT COUNT(*) AS n FROM leases WHERE id = ?`, lease.lease_id)).toBe(1);
  });
});
