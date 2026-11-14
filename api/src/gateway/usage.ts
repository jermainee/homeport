import type { Context } from "hono";
import { ID, UUID_V7, isCount, isIpAddress, parseDateTime, uuidV7Timestamp } from "../formats";
import { hasOnlyFields, invalidRequest } from "../http";
import { chargeUsage } from "../ledger";
import type { GatewayVariables } from "./auth";
import { EVENT_MAX_AGE_MS, readEventBatch } from "./events";

type GatewayContext = Context<{ Bindings: Env; Variables: GatewayVariables }>;

/** One usage event, checked against the contract's UsageEvent. */
interface UsageEvent {
  event_id: string;
  session_id: string;
  peer_id: string;
  line_id: string;
  lease_id: string;
  seq: number;
  bytes_up: number;
  bytes_down: number;
  period_start: number;
  period_end: number;
  final: boolean;
  session_start: { started_at: number; exit_ip: string } | null;
}

export type Reason =
  | "unknown_peer"
  | "unknown_line"
  | "unknown_lease"
  | "lease_mismatch"
  | "invalid_period"
  | "seq_conflict"
  | "expired";

type Result = { event_id: string; status: "applied" | "duplicate" } | { event_id: string; status: "rejected"; reason: Reason };

const EVENT_FIELDS = [
  "event_id",
  "session_id",
  "peer_id",
  "line_id",
  "lease_id",
  "seq",
  "bytes_up",
  "bytes_down",
  "period_start",
  "period_end",
  "final",
  "session_start",
];

function parseUsageEvent(value: unknown): UsageEvent | string {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return "not an object";
  const event = value as Record<string, unknown>;
  if (!hasOnlyFields(event, EVENT_FIELDS)) return "unknown field";
  const { event_id, session_id, peer_id, line_id, lease_id, seq, bytes_up, bytes_down, final } = event;
  if (typeof event_id !== "string" || !UUID_V7.test(event_id)) return "event_id must be a UUIDv7";
  if (typeof session_id !== "string" || !UUID_V7.test(session_id)) return "session_id must be a UUIDv7";
  for (const [name, id] of [["peer_id", peer_id], ["line_id", line_id], ["lease_id", lease_id]] as const) {
    if (typeof id !== "string" || !ID.test(id)) return `${name} must be an id`;
  }
  if (!isCount(seq) || !isCount(bytes_up) || !isCount(bytes_down)) return "seq and byte counts must be whole numbers from 0";
  if (!isCount(bytes_up + bytes_down)) return "bytes_up + bytes_down is too large";
  if (typeof final !== "boolean") return "final must be a boolean";
  const periodStart = parseDateTime(event["period_start"]);
  const periodEnd = parseDateTime(event["period_end"]);
  if (periodStart === null || periodEnd === null) return "period_start and period_end must be timestamps";

  let sessionStart: UsageEvent["session_start"] = null;
  if ((seq === 0) !== ("session_start" in event)) return "session_start belongs on seq 0, and only there";
  if (seq === 0) {
    const start = event["session_start"];
    if (typeof start !== "object" || start === null || !hasOnlyFields(start as Record<string, unknown>, ["started_at", "exit_ip"])) {
      return "session_start must be {started_at, exit_ip}";
    }
    const startedAt = parseDateTime((start as Record<string, unknown>)["started_at"]);
    const exitIp = (start as Record<string, unknown>)["exit_ip"];
    if (startedAt === null || !isIpAddress(exitIp)) return "session_start needs a timestamp and an IP address";
    sessionStart = { started_at: startedAt, exit_ip: exitIp };
  }
  return {
    event_id,
    session_id,
    peer_id: peer_id as string,
    line_id: line_id as string,
    lease_id: lease_id as string,
    seq,
    bytes_up,
    bytes_down,
    period_start: periodStart,
    period_end: periodEnd,
    final,
    session_start: sessionStart,
  };
}

interface StoredEvent {
  event_id: string;
  session_id: string;
  peer_id: string;
  line_id: string;
  lease_id: string;
  seq: number;
  bytes_up: number;
  bytes_down: number;
  period_start: number;
  period_end: number;
  final: number;
}

interface PeerRow {
  id: string;
  member_id: string;
}

interface LineRow {
  id: string;
  owner_member_id: string;
}

interface LeaseRow {
  id: string;
  peer_id: string;
  line_id: string;
  session_id: string;
}

interface SessionRow {
  session_id: string;
  peer_id: string;
  line_id: string;
  next_seq: number;
  last_period_end: number;
  ended: number;
}

/** The same event as one stored before: a retry, not new usage. */
function sameEvent(stored: StoredEvent, event: UsageEvent): boolean {
  return (
    stored.session_id === event.session_id &&
    stored.peer_id === event.peer_id &&
    stored.line_id === event.line_id &&
    stored.lease_id === event.lease_id &&
    stored.seq === event.seq &&
    stored.bytes_up === event.bytes_up &&
    stored.bytes_down === event.bytes_down &&
    stored.period_start === event.period_start &&
    stored.period_end === event.period_end &&
    stored.final === (event.final ? 1 : 0)
  );
}

/**
 * POST /v1/gateway/usage (contract reportUsage). Applies each event exactly
 * once, in request order, and answers each.
 *
 * An applied event is charged to the meter of its peer on its line
 * (meter_totals, one per peer and line): the user pays floor(total × 10 /
 * 10^6) minus what the meter already charged, the owner of the event's line
 * earns at 5 and the platform keeps the rest (chargeUsage). The event's
 * line is its lease's: the peer's own line, or a line that took it over
 * while it was offline. The charge counts against the event's lease. The
 * event and the session log (usage_sessions: member, line, start, end,
 * bytes; never a destination) are written in the charge's batch, so a
 * retry after a failure finds all of it or none of it.
 *
 * Usage past a lease's bytes, after it expired, or of a peer deleted since
 * is still charged: the gateway is the meter of record.
 */
export async function reportUsage(c: GatewayContext): Promise<Response> {
  const batch = await readEventBatch(c.req.raw, parseUsageEvent);
  if ("error" in batch) return invalidRequest(c, batch.error);
  const { events } = batch;
  const db = c.env.DB;
  const gatewayId = c.get("gateway").id;
  const now = Date.now();

  const ids = (pick: (event: UsageEvent) => string) => JSON.stringify([...new Set(events.map(pick))]);
  const [storedResult, peersResult, linesResult, leasesResult, sessionsResult] = await db.batch([
    db
      .prepare(
        `SELECT event_id, session_id, peer_id, line_id, lease_id, seq, bytes_up, bytes_down, period_start, period_end, final
           FROM usage_events WHERE event_id IN (SELECT value FROM json_each(?))`,
      )
      .bind(ids((event) => event.event_id)),
    db
      .prepare(
        `SELECT p.id, p.member_id FROM wireguard_peers p WHERE p.id IN (SELECT value FROM json_each(?))`,
      )
      .bind(ids((event) => event.peer_id)),
    db
      .prepare(`SELECT id, member_id AS owner_member_id FROM lines WHERE id IN (SELECT value FROM json_each(?))`)
      .bind(ids((event) => event.line_id)),
    db
      .prepare(`SELECT id, peer_id, line_id, session_id FROM leases WHERE gateway_id = ? AND id IN (SELECT value FROM json_each(?))`)
      .bind(gatewayId, ids((event) => event.lease_id)),
    db
      .prepare(
        `SELECT session_id, peer_id, line_id, next_seq, last_period_end, ended
           FROM usage_sessions WHERE session_id IN (SELECT value FROM json_each(?))`,
      )
      .bind(ids((event) => event.session_id)),
  ]);
  const stored = new Map(((storedResult?.results ?? []) as StoredEvent[]).map((row) => [row.event_id, row]));
  const peers = new Map(((peersResult?.results ?? []) as PeerRow[]).map((row) => [row.id, row]));
  const lines = new Map(((linesResult?.results ?? []) as LineRow[]).map((row) => [row.id, row]));
  const leases = new Map(((leasesResult?.results ?? []) as LeaseRow[]).map((row) => [row.id, row]));
  const sessions = new Map(((sessionsResult?.results ?? []) as SessionRow[]).map((row) => [row.session_id, row]));

  const results: Result[] = [];
  for (const event of events) {
    const outcome = await applyUsageEvent(db, gatewayId, event, { stored, peers, lines, leases, sessions }, now);
    results.push(
      typeof outcome === "string"
        ? { event_id: event.event_id, status: outcome }
        : { event_id: event.event_id, status: "rejected", reason: outcome.reason },
    );
  }

  // Counts only: no member, line or destination in logs.
  console.log(
    JSON.stringify({
      event: "usage_batch",
      gateway: gatewayId,
      applied: results.filter((result) => result.status === "applied").length,
      duplicate: results.filter((result) => result.status === "duplicate").length,
      rejected: results.filter((result) => result.status === "rejected").length,
    }),
  );
  return c.json({ results }, 200);
}

interface Known {
  stored: Map<string, StoredEvent>;
  peers: Map<string, PeerRow>;
  lines: Map<string, LineRow>;
  leases: Map<string, LeaseRow>;
  sessions: Map<string, SessionRow>;
}

/** Applies one event, or says why not. Updates `known` with what it applied, for the batch's later events. */
async function applyUsageEvent(
  db: D1Database,
  gatewayId: string,
  event: UsageEvent,
  known: Known,
  now: number,
): Promise<"applied" | "duplicate" | { reason: Reason }> {
  if (uuidV7Timestamp(event.event_id) < now - EVENT_MAX_AGE_MS) return { reason: "expired" };
  const previous = known.stored.get(event.event_id);
  if (previous) return sameEvent(previous, event) ? "duplicate" : { reason: "seq_conflict" };

  const peer = known.peers.get(event.peer_id);
  if (!peer) return { reason: "unknown_peer" };
  const line = known.lines.get(event.line_id);
  if (!line) return { reason: "unknown_line" };
  const lease = known.leases.get(event.lease_id);
  if (!lease) return { reason: "unknown_lease" };
  if (lease.peer_id !== event.peer_id || lease.session_id !== event.session_id || lease.line_id !== event.line_id) {
    return { reason: "lease_mismatch" };
  }

  const session = known.sessions.get(event.session_id);
  if (event.seq === 0 ? session !== undefined : !session || session.ended === 1 || session.next_seq !== event.seq) {
    return { reason: "seq_conflict" };
  }
  if (session && (session.peer_id !== event.peer_id || session.line_id !== event.line_id)) return { reason: "seq_conflict" };
  if (event.period_end <= event.period_start || (session && event.period_start !== session.last_period_end)) {
    return { reason: "invalid_period" };
  }

  const charged = await chargeUsage(db, {
    meterId: `${event.peer_id}/${event.line_id}`,
    payerMemberId: peer.member_id,
    payeeMemberId: line.owner_member_id,
    bytes: event.bytes_up + event.bytes_down,
    usageKey: event.event_id,
    reservationId: event.lease_id,
    now,
    alsoWrite: (guard) => [
      db
        .prepare(
          `INSERT INTO usage_events (event_id, gateway_id, session_id, peer_id, line_id, lease_id, seq, bytes_up, bytes_down,
                                     period_start, period_end, final, received_at)
           SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE ${guard.sql}`,
        )
        .bind(
          event.event_id,
          gatewayId,
          event.session_id,
          event.peer_id,
          event.line_id,
          event.lease_id,
          event.seq,
          event.bytes_up,
          event.bytes_down,
          event.period_start,
          event.period_end,
          event.final ? 1 : 0,
          now,
          ...guard.binds,
        ),
      event.session_start
        ? db
            .prepare(
              `INSERT INTO usage_sessions (session_id, gateway_id, peer_id, member_id, line_id, owner_member_id, exit_ip,
                                           started_at, last_period_end, next_seq, bytes_up, bytes_down, ended, updated_at)
               SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ? WHERE ${guard.sql}`,
            )
            .bind(
              event.session_id,
              gatewayId,
              event.peer_id,
              peer.member_id,
              event.line_id,
              line.owner_member_id,
              event.session_start.exit_ip,
              event.session_start.started_at,
              event.period_end,
              event.bytes_up,
              event.bytes_down,
              event.final ? 1 : 0,
              now,
              ...guard.binds,
            )
        : // The view's trigger aborts the batch, charge included, unless the
          // session still expects this seq: the log never misses a charge.
          db
            .prepare(
              `INSERT INTO usage_session_steps (session_id, seq, period_end, bytes_up, bytes_down, final, updated_at)
               SELECT ?, ?, ?, ?, ?, ?, ? WHERE ${guard.sql}`,
            )
            .bind(
              event.session_id,
              event.seq,
              event.period_end,
              event.bytes_up,
              event.bytes_down,
              event.final ? 1 : 0,
              now,
              ...guard.binds,
            ),
    ],
  });

  known.stored.set(event.event_id, {
    event_id: event.event_id,
    session_id: event.session_id,
    peer_id: event.peer_id,
    line_id: event.line_id,
    lease_id: event.lease_id,
    seq: event.seq,
    bytes_up: event.bytes_up,
    bytes_down: event.bytes_down,
    period_start: event.period_start,
    period_end: event.period_end,
    final: event.final ? 1 : 0,
  });
  // A duplicate here was applied by a concurrent request since this one
  // read: the session moved on all the same.
  known.sessions.set(event.session_id, {
    session_id: event.session_id,
    peer_id: event.peer_id,
    line_id: event.line_id,
    next_seq: event.seq + 1,
    last_period_end: event.period_end,
    ended: event.final ? 1 : 0,
  });
  return charged.status === "duplicate" ? "duplicate" : "applied";
}
