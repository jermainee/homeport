import type { Context } from "hono";
import { ID, SEMVER, UUID_V7, isIpAddress, parseDateTime, uuidV7Timestamp } from "../formats";
import { hasOnlyFields, invalidRequest } from "../http";
import type { GatewayVariables } from "./auth";
import { EVENT_MAX_AGE_MS, readEventBatch } from "./events";

type GatewayContext = Context<{ Bindings: Env; Variables: GatewayVariables }>;

/** One line event, checked against the contract's LineEvent. */
interface LineEvent {
  event_id: string;
  line_id: string;
  type: "online" | "offline" | "tampered";
  /** For a tampered event: the canary check that caught the line. */
  check: "http" | "dns" | null;
  observed_ip: string;
  agent_version: string;
  at: number;
}

const FIELDS = ["event_id", "line_id", "type", "check", "observed_ip", "agent_version", "at"];

function parseLineEvent(value: unknown): LineEvent | string {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return "not an object";
  const event = value as Record<string, unknown>;
  if (!hasOnlyFields(event, FIELDS)) return "unknown field";
  const { event_id, line_id, type, check, observed_ip, agent_version } = event;
  if (typeof event_id !== "string" || !UUID_V7.test(event_id)) return "event_id must be a UUIDv7";
  if (typeof line_id !== "string" || !ID.test(line_id)) return "line_id must be an id";
  if (type !== "online" && type !== "offline" && type !== "tampered") return "type must be online, offline or tampered";
  if (type === "tampered" ? check !== "http" && check !== "dns" : "check" in event) {
    return "check must be http or dns for a tampered event, and absent for the others";
  }
  if (!isIpAddress(observed_ip)) return "observed_ip must be an IP address";
  if (typeof agent_version !== "string" || !SEMVER.test(agent_version)) return "agent_version must be a semantic version";
  const at = parseDateTime(event["at"]);
  if (at === null) return "at must be a timestamp";
  return { event_id, line_id, type, check: type === "tampered" ? (check as "http" | "dns") : null, observed_ip, agent_version, at };
}

type Result =
  | { event_id: string; status: "applied" | "duplicate" }
  | { event_id: string; status: "rejected"; reason: "unknown_line" | "expired" };

/**
 * POST /v1/gateway/line-events (contract reportLineEvents). Marks this
 * gateway's lines online or offline, each event once. An event older than
 * the line's newest applied one is recorded but changes nothing, since
 * events can arrive out of order. A tampered event also pauses the line, for
 * good: servedLineSql leaves it out of every gateway's config, and nobody
 * can pick it, until someone reviews it. The agent's IP (observed_ip) is
 * checked but not stored: nothing needs it yet.
 */
export async function reportLineEvents(c: GatewayContext): Promise<Response> {
  const batch = await readEventBatch(c.req.raw, parseLineEvent);
  if ("error" in batch) return invalidRequest(c, batch.error);
  const { events } = batch;
  const db = c.env.DB;
  const gatewayId = c.get("gateway").id;
  const now = Date.now();

  const [storedResult, linesResult] = await db.batch([
    db
      .prepare(`SELECT event_id FROM line_events WHERE event_id IN (SELECT value FROM json_each(?))`)
      .bind(JSON.stringify(events.map((event) => event.event_id))),
    db
      .prepare(`SELECT id FROM lines WHERE gateway_id = ? AND id IN (SELECT value FROM json_each(?))`)
      .bind(gatewayId, JSON.stringify(events.map((event) => event.line_id))),
  ]);
  const seen = new Set(((storedResult?.results ?? []) as { event_id: string }[]).map((row) => row.event_id));
  const lines = new Set(((linesResult?.results ?? []) as { id: string }[]).map((row) => row.id));

  const results: Result[] = [];
  const statements: D1PreparedStatement[] = [];
  for (const event of events) {
    if (uuidV7Timestamp(event.event_id) < now - EVENT_MAX_AGE_MS) {
      results.push({ event_id: event.event_id, status: "rejected", reason: "expired" });
    } else if (seen.has(event.event_id)) {
      results.push({ event_id: event.event_id, status: "duplicate" });
    } else if (!lines.has(event.line_id)) {
      results.push({ event_id: event.event_id, status: "rejected", reason: "unknown_line" });
    } else {
      seen.add(event.event_id);
      results.push({ event_id: event.event_id, status: "applied" });
      statements.push(
        db
          .prepare(
            `INSERT INTO line_events (event_id, gateway_id, line_id, type, tamper_check, agent_version, at, received_at)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
          )
          .bind(event.event_id, gatewayId, event.line_id, event.type, event.check, event.agent_version, event.at, now),
        db
          .prepare(
            `UPDATE lines SET online = ?, online_changed_at = ?
              WHERE id = ? AND (online_changed_at IS NULL OR online_changed_at <= ?)`,
          )
          .bind(event.type === "online" ? 1 : 0, event.at, event.line_id, event.at),
      );
      if (event.type === "tampered") {
        statements.push(
          db
            .prepare(`UPDATE lines SET paused_at = ?, paused_reason = ?, updated_at = ? WHERE id = ? AND paused_at IS NULL`)
            .bind(event.at, `tampered_${event.check}`, now, event.line_id),
        );
      }
    }
  }
  // All or nothing: a failure answers 500 and the gateway sends the batch again.
  if (statements.length > 0) await db.batch(statements);
  return c.json({ results }, 200);
}
