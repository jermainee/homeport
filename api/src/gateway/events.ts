import { hasOnlyFields, readJsonObject } from "../http";

/** An event whose event_id is older than this is rejected as expired (contract). */
export const EVENT_MAX_AGE_MS = 72 * 60 * 60 * 1000;

/** Events per batch (contract UsageBatch, LineEventBatch). */
export const MAX_EVENTS_PER_BATCH = 100;

/**
 * The events of a batch body (`{events: [...]}`), or a message saying why
 * the body does not match the contract. `parse` checks one event.
 */
export async function readEventBatch<T>(
  request: Request,
  parse: (event: unknown) => T | string,
): Promise<{ events: T[] } | { error: string }> {
  const body = await readJsonObject(request);
  if (!body || !hasOnlyFields(body, ["events"])) return { error: "body must be {events: [...]}" };
  const events = body["events"];
  if (!Array.isArray(events) || events.length === 0 || events.length > MAX_EVENTS_PER_BATCH) {
    return { error: `events must hold 1 to ${MAX_EVENTS_PER_BATCH} events` };
  }
  const parsed: T[] = [];
  for (const [index, event] of events.entries()) {
    const result = parse(event);
    if (typeof result === "string") return { error: `event ${index}: ${result}` };
    parsed.push(result);
  }
  return { events: parsed };
}
