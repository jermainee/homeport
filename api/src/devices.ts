import { Hono } from "hono";
import type { SessionVariables } from "./auth/middleware";
import { apiError, hasOnlyFields, invalidRequest } from "./http";
import { type IdempotentVariables, memberIdempotent } from "./idempotency";
import { KEY32, ed25519SpkiSha256, isName } from "./keys";

const PLATFORMS = ["linux", "macos", "windows"];

/** New devices a member may register per DEVICE_WINDOW_MS; registering a known key again is free. */
export const MAX_NEW_DEVICES_PER_WINDOW = 10;
export const DEVICE_WINDOW_MS = 24 * 60 * 60 * 1000;

/**
 * Sharing agents' keys (contract operation registerDevice). Registering
 * needs no identity check: the app registers its agent key when it is
 * installed, and creating a line is what needs one.
 */
export const devicesApi = new Hono<{ Bindings: Env; Variables: SessionVariables & IdempotentVariables }>();

devicesApi.post("/", memberIdempotent, async (c) => {
  const body = c.get("body");
  if (!hasOnlyFields(body, ["request_id", "name", "platform", "agent_public_key"])) {
    return invalidRequest(c, "unknown field");
  }
  const { name, platform, agent_public_key: agentKey } = body;
  if (!isName(name)) return invalidRequest(c, "name must have 1 to 64 characters");
  if (typeof platform !== "string" || !PLATFORMS.includes(platform)) {
    return invalidRequest(c, "platform must be linux, macos or windows");
  }
  if (typeof agentKey !== "string" || !KEY32.test(agentKey)) {
    return invalidRequest(c, "agent_public_key must be a 32-byte key in standard base64");
  }

  const db = c.env.DB;
  const memberId = c.get("member").id;
  const now = Date.now();
  const [known, recent] = await db.batch<{ member_id?: string; count?: number; oldest?: number | null }>([
    db.prepare(`SELECT member_id FROM devices WHERE agent_public_key = ?`).bind(agentKey),
    db
      .prepare(`SELECT COUNT(*) AS count, MIN(created_at) AS oldest FROM devices WHERE member_id = ? AND created_at > ?`)
      .bind(memberId, now - DEVICE_WINDOW_MS),
  ]);
  const recentCount = recent?.results[0]?.count ?? 0;
  if (!known?.results[0] && recentCount >= MAX_NEW_DEVICES_PER_WINDOW) {
    const freeAt = (recent?.results[0]?.oldest ?? now) + DEVICE_WINDOW_MS;
    c.header("retry-after", String(Math.max(1, Math.ceil((freeAt - now) / 1000))));
    return apiError(c, 429, "rate_limited", `at most ${MAX_NEW_DEVICES_PER_WINDOW} new devices a day`);
  }

  const spkiSha256 = await ed25519SpkiSha256(agentKey);
  const inserted = await db
    .prepare(
      `INSERT INTO devices (id, member_id, name, platform, agent_public_key, spki_sha256, created_at)
       VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
    )
    .bind(crypto.randomUUID(), memberId, name, platform, agentKey, spkiSha256, now)
    .run();
  const device = await db
    .prepare(`SELECT id, member_id, spki_sha256 FROM devices WHERE agent_public_key = ?`)
    .bind(agentKey)
    .first<{ id: string; member_id: string; spki_sha256: string }>();
  if (!device) throw new Error("device missing after insert");
  if (device.member_id !== memberId) return apiError(c, 409, "agent_key_taken");
  return c.json({ device_id: device.id, spki_sha256: device.spki_sha256 }, inserted.meta.changes === 1 ? 201 : 200);
});
