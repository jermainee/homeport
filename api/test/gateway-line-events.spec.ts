import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { expectContract } from "./contract";
import { announcedGateway, gatewayCall, lineOn } from "./gateway-fixtures";
import { uuidv7 } from "./helpers";

interface Results {
  results: { event_id: string; status: string; reason?: string }[];
}

function lineEvent(lineId: string, type: "online" | "offline", at: number, eventId = uuidv7()) {
  return {
    event_id: eventId,
    line_id: lineId,
    type,
    observed_ip: "203.0.113.47",
    agent_version: "0.1.0",
    at: new Date(at).toISOString(),
  };
}

async function report(token: string, events: unknown[]): Promise<Results> {
  const response = await gatewayCall(token, "POST", "/v1/gateway/line-events", { events });
  expect(response.status).toBe(200);
  return expectContract<Results>("POST", "/v1/gateway/line-events", response);
}

async function online(lineId: string): Promise<number | undefined> {
  return (await env.DB.prepare(`SELECT online FROM lines WHERE id = ?`).bind(lineId).first<{ online: number }>())?.online;
}

describe("POST /v1/gateway/line-events", () => {
  it("marks a line online and offline", async () => {
    const { gatewayId, token } = await announcedGateway();
    const { lineId } = await lineOn(gatewayId);
    const now = Date.now();

    const up = await report(token, [lineEvent(lineId, "online", now - 2000)]);
    const wasOnline = await online(lineId);
    const down = await report(token, [lineEvent(lineId, "offline", now - 1000)]);

    expect(up.results.map((r) => r.status)).toEqual(["applied"]);
    expect(down.results.map((r) => r.status)).toEqual(["applied"]);
    expect(wasOnline).toBe(1);
    expect(await online(lineId)).toBe(0);
  });

  it("applies each event once", async () => {
    const { gatewayId, token } = await announcedGateway();
    const { lineId } = await lineOn(gatewayId);
    const event = lineEvent(lineId, "online", Date.now());

    const results = await report(token, [event, event]);

    expect(results.results.map((r) => r.status)).toEqual(["applied", "duplicate"]);
  });

  it("keeps the newest state when events arrive out of order", async () => {
    const { gatewayId, token } = await announcedGateway();
    const { lineId } = await lineOn(gatewayId);
    const now = Date.now();

    await report(token, [lineEvent(lineId, "online", now)]);
    const late = await report(token, [lineEvent(lineId, "offline", now - 5000)]);

    expect(late.results.map((r) => r.status)).toEqual(["applied"]);
    expect(await online(lineId)).toBe(1);
  });

  it("rejects events for unknown lines and other gateways' lines, and old events", async () => {
    const mine = await announcedGateway();
    const other = await announcedGateway();
    const { lineId: mineLine } = await lineOn(mine.gatewayId);
    const { lineId: otherLine } = await lineOn(other.gatewayId);
    const now = Date.now();

    const results = await report(mine.token, [
      lineEvent(crypto.randomUUID(), "online", now),
      lineEvent(otherLine, "online", now),
      lineEvent(mineLine, "online", now, uuidv7(now - 73 * 60 * 60 * 1000)),
    ]);

    expect(results.results.map((r) => `${r.status}:${r.reason ?? ""}`)).toEqual([
      "rejected:unknown_line",
      "rejected:unknown_line",
      "rejected:expired",
    ]);
    expect(await online(otherLine)).toBe(0);
    expect(await online(mineLine)).toBe(0);
  });

  it("rejects a batch that does not match the contract with 400", async () => {
    const { gatewayId, token } = await announcedGateway();
    const { lineId } = await lineOn(gatewayId);
    const valid = lineEvent(lineId, "online", Date.now());

    for (const events of [[], [{ ...valid, type: "paused" }], [{ ...valid, agent_version: "v1" }], [{ ...valid, extra: 1 }]]) {
      const response = await gatewayCall(token, "POST", "/v1/gateway/line-events", { events });
      expect(response.status).toBe(400);
      await expectContract("POST", "/v1/gateway/line-events", response);
    }
  });
});
