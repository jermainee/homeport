/**
 * Raw usage and line events are kept this long after they arrive, so a
 * retry within the contract's 7 days is answered duplicate. The ledger,
 * meter totals and the session log stay.
 */
export const RAW_EVENT_RETENTION_MS = 7 * 24 * 60 * 60 * 1000;

/** Rows one statement deletes at most, so no single delete runs long. */
const CHUNK = 5_000;

/**
 * Deletes rows nothing needs any more: raw usage and line events, and the
 * usage charges that dedupe their events, after RAW_EVENT_RETENTION_MS
 * (events older than 72 hours are rejected as expired before they reach
 * a charge); and idempotency records past their expiry. Runs from the
 * cron trigger.
 */
export async function prune(db: D1Database, now: number): Promise<Record<string, number>> {
  const cutoff = now - RAW_EVENT_RETENTION_MS;
  const targets: [string, string, number][] = [
    ["usage_events", `SELECT event_id FROM usage_events WHERE received_at < ? LIMIT ${CHUNK}`, cutoff],
    ["usage_charges", `SELECT usage_key FROM usage_charges WHERE created_at < ? LIMIT ${CHUNK}`, cutoff],
    ["line_events", `SELECT event_id FROM line_events WHERE received_at < ? LIMIT ${CHUNK}`, cutoff],
    ["idempotency_records", `SELECT request_id FROM idempotency_records WHERE expires_at < ? LIMIT ${CHUNK}`, now],
  ];
  const deleted: Record<string, number> = {};
  for (const [table, select, before] of targets) {
    const key = select.split(" ")[1] ?? "";
    deleted[table] = 0;
    for (;;) {
      const result = await db.prepare(`DELETE FROM ${table} WHERE ${key} IN (${select})`).bind(before).run();
      deleted[table] += result.meta.changes;
      if (result.meta.changes < CHUNK) break;
    }
  }
  return deleted;
}
