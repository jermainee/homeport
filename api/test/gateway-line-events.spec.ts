import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { expectContract } from "./contract";
import { announcedGateway, gatewayCall, lineOn, servedPeer } from "./gateway-fixtures";
import { uuidv7 } from "./helpers";
import { randomKey, send, verifiedMember } from "./share-fixtures";

interface Results {
  results: { event_id: string; status: string; reason?: string }[];
}

function lineEvent(lineId: string, type: "online" | "offline" | "tampered", at: number, eventId = uuidv7()) {
  return {
    event_id: eventId,
    line_id: lineId,
    type,
    observed_ip: "203.0.113.47",
    agent_version: "0.1.0",
    at: new Date(at).toISOString(),
    ...(type === "tampered" ? { check: "http" } : {}),
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

    const tampered = lineEvent(lineId, "tampered", Date.now());
    for (const events of [
      [],
      [{ ...valid, type: "paused" }],
      [{ ...valid, agent_version: "v1" }],
      [{ ...valid, extra: 1 }],
      [{ ...valid, check: "http" }],
      [{ ...tampered, check: "tls" }],
      [{ ...tampered, check: undefined }],
    ]) {
      const response = await gatewayCall(token, "POST", "/v1/gateway/line-events", { events });
      expect(response.status).toBe(400);
      await expectContract("POST", "/v1/gateway/line-events", response);
    }
  });

  it("pauses a line reported tampered, so no gateway serves it and nobody can pick it", async () => {
    const { gateway, lineId, user, peer } = await servedPeer();
    const before = await config(gateway.token);
    expect(before.lines.map((l) => l.line_id)).toContain(lineId);

    const results = await report(gateway.token, [{ ...lineEvent(lineId, "tampered", Date.now()), check: "dns" }]);

    expect(results.results.map((r) => r.status)).toEqual(["applied"]);
    const after = await config(gateway.token);
    expect(after.lines.map((l) => l.line_id)).not.toContain(lineId);
    expect(after.peers.map((p) => p.peer_id)).not.toContain(peer.peer_id);
    expect(after.etag).not.toBe(before.etag);
    const row = await env.DB.prepare(`SELECT paused_at, paused_reason, online FROM lines WHERE id = ?`)
      .bind(lineId)
      .first<{ paused_at: number | null; paused_reason: string | null; online: number }>();
    expect(row?.paused_at).not.toBeNull();
    expect(row?.paused_reason).toBe("tampered_dns");
    expect(row?.online).toBe(0);
    const stored = await env.DB.prepare(`SELECT type, tamper_check AS "check" FROM line_events WHERE line_id = ?`)
      .bind(lineId)
      .first<{ type: string; check: string }>();
    expect(stored).toEqual({ type: "tampered", check: "dns" });

    const buyer = await verifiedMember();
    const available = await send("GET", "/v1/lines/available", buyer.sessionToken);
    const { lines } = await available.json<{ lines: { line_id: string }[] }>();
    expect(lines.map((l) => l.line_id)).not.toContain(lineId);
    const created = await send("POST", "/v1/wireguard/peers", user.sessionToken, {
      request_id: uuidv7(),
      line_id: lineId,
      name: "Laptop",
      public_key: randomKey(),
    });
    expect(created.status).toBe(404);
  });

  it("keeps a tampered line paused when its agent comes back online", async () => {
    const { gateway, lineId } = await servedPeer();
    const now = Date.now();

    await report(gateway.token, [lineEvent(lineId, "tampered", now - 2000), lineEvent(lineId, "offline", now - 2000)]);
    await report(gateway.token, [lineEvent(lineId, "online", now - 1000)]);

    expect((await config(gateway.token)).lines.map((l) => l.line_id)).not.toContain(lineId);
    const row = await env.DB.prepare(`SELECT paused_at FROM lines WHERE id = ?`).bind(lineId).first<{ paused_at: number | null }>();
    expect(row?.paused_at).toBe(now - 2000);
  });
});

async function config(token: string) {
  const response = await gatewayCall(token, "GET", "/v1/gateway/config");
  expect(response.status).toBe(200);
  return response.json<{ etag: string; lines: { line_id: string }[]; peers: { peer_id: string }[] }>();
}
