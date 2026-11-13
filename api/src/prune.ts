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
  // Table, key column, time column, and the time before which rows go.
  const targets: [string, string, string, number][] = [
    ["usage_events", "event_id", "received_at", cutoff],
    ["usage_charges", "usage_key", "created_at", cutoff],
    ["line_events", "event_id", "received_at", cutoff],
    ["idempotency_records", "request_id", "expires_at", now],
  ];
  const deleted: Record<string, number> = {};
  for (const [table, key, time, before] of targets) {
    deleted[table] = 0;
    for (;;) {
      const result = await db
        .prepare(`DELETE FROM ${table} WHERE ${key} IN (SELECT ${key} FROM ${table} WHERE ${time} < ? LIMIT ${CHUNK})`)
        .bind(before)
        .run();
      deleted[table] += result.meta.changes;
      if (result.meta.changes < CHUNK) break;
    }
  }
  return deleted;
}
