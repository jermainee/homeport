import { Hono } from "hono";
import type { SessionVariables } from "../auth/middleware";
import { apiError, hasOnlyFields, invalidRequest, readJsonObject } from "../http";
import { type IdempotentVariables, verifiedMemberIdempotent } from "../idempotency";
import { requireVerifiedMember } from "../identity/require-verified";
import { lineHasRoomSql, ownerIsLinkedSql } from "./rules";

/**
 * Lines a member shares and lines they can use (contract operations
 * createLine, updateLine, listAvailableLines). All need a verified identity.
 */
export const linesApi = new Hono<{ Bindings: Env; Variables: SessionVariables & IdempotentVariables }>();

interface LineWithGateway {
  id: string;
  quic_endpoint: string;
  quic_server_name: string;
  quic_spki_sha256: string;
}

linesApi.post("/", verifiedMemberIdempotent, async (c) => {
  const body = c.get("body");
  if (!hasOnlyFields(body, ["request_id", "device_id"])) return invalidRequest(c, "unknown field");
  const deviceId = body["device_id"];
  if (typeof deviceId !== "string") return invalidRequest(c, "device_id is required");

  const db = c.env.DB;
  const memberId = c.get("member").id;
  const device = await db
    .prepare(`SELECT id FROM devices WHERE id = ? AND member_id = ?`)
    .bind(deviceId, memberId)
    .first();
  if (!device) return apiError(c, 404, "not_found");

  // The gateway that announced itself last, unless its token was revoked.
  const gateway = await db
    .prepare(
      `SELECT g.id FROM gateways g
        WHERE NOT EXISTS (SELECT 1 FROM gateway_credentials c WHERE c.gateway_id = g.id AND c.revoked_at IS NOT NULL)
        ORDER BY g.updated_at DESC LIMIT 1`,
    )
    .first<{ id: string }>();
  if (!gateway) return apiError(c, 503, "gateway_unavailable", "no gateway has announced itself yet");

  // Where the line is, as Cloudflare sees the device's request.
  const cf = c.req.raw.cf as IncomingRequestCfProperties | undefined;
  const now = Date.now();
  const inserted = await db
    .prepare(
      `INSERT INTO lines (id, device_id, member_id, gateway_id, country, city, isp, created_at, updated_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (device_id) DO NOTHING`,
    )
    .bind(
      crypto.randomUUID(),
      deviceId,
      memberId,
      gateway.id,
      countryCode(cf?.country),
      cf?.city ?? null,
      cf?.asOrganization ?? null,
      now,
      now,
    )
    .run();
  const line = await db
    .prepare(
      `SELECT l.id, g.quic_endpoint, g.quic_server_name, g.quic_spki_sha256
         FROM lines l JOIN gateways g ON g.id = l.gateway_id WHERE l.device_id = ?`,
    )
    .bind(deviceId)
    .first<LineWithGateway>();
  if (!line) throw new Error("line missing after insert");
  return c.json(
    {
      line_id: line.id,
      gateway: {
        quic_endpoint: line.quic_endpoint,
        server_name: line.quic_server_name,
        spki_sha256: line.quic_spki_sha256,
      },
    },
    inserted.meta.changes === 1 ? 201 : 200,
  );
});

linesApi.get("/available", requireVerifiedMember, async (c) => {
  // A line needs a known country to be listed (the contract requires one),
  // an owner who is not suspended, and no pause for tampering.
  const { results } = await c.env.DB.prepare(
    `SELECT l.id, l.country, l.city, l.isp, l.ipv6, l.online
       FROM lines l JOIN members owner ON owner.id = l.member_id AND owner.status = 'active'
      WHERE l.sharing_enabled = 1 AND l.paused_at IS NULL AND l.country IS NOT NULL
        AND NOT ${ownerIsLinkedSql("?1", "l.member_id")}
        AND ${lineHasRoomSql("l.id", "?1")}
      ORDER BY l.created_at`,
  )
    .bind(c.get("member").id)
    .all<{ id: string; country: string; city: string | null; isp: string | null; ipv6: number; online: number }>();
  return c.json({
    lines: results.map((line) => ({
      line_id: line.id,
      country: line.country,
      city: line.city,
      isp: line.isp,
      ipv6: line.ipv6 === 1,
      online: line.online === 1,
    })),
  });
});

linesApi.patch("/:id", requireVerifiedMember, async (c) => {
  const body = await readJsonObject(c.req.raw);
  const sharing = body?.["sharing_enabled"];
  if (!body || !hasOnlyFields(body, ["sharing_enabled"]) || typeof sharing !== "boolean") {
    return invalidRequest(c, "sharing_enabled must be true or false");
  }
  const updated = await c.env.DB.prepare(
    `UPDATE lines SET sharing_enabled = ?, updated_at = ? WHERE id = ? AND member_id = ? RETURNING id`,
  )
    .bind(sharing ? 1 : 0, Date.now(), c.req.param("id"), c.get("member").id)
    .first<{ id: string }>();
  if (!updated) return apiError(c, 404, "not_found");
  return c.json({ line_id: updated.id, sharing_enabled: sharing });
});

/** An ISO 3166-1 alpha-2 code, or null. Cloudflare sends "XX" or "T1" (Tor) when it does not know. */
function countryCode(value: string | null | undefined): string | null {
  return value && /^[A-Z]{2}$/.test(value) && value !== "XX" ? value : null;
}
