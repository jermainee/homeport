import { createMiddleware } from "hono/factory";
import { bearerToken, unauthorized } from "../auth/middleware";
import { randomToken, sha256Hex } from "../crypto";

export interface GatewayVariables {
  /** The gateway the request's token belongs to. */
  gateway: { id: string };
}

/** `hpgw_<gatewayId>.<secret>`: a lowercase UUID, then 32 random bytes in base64url. */
const GATEWAY_TOKEN = /^hpgw_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.([A-Za-z0-9_-]{43})$/;

/** Compared when a gateway id has no credential, so an unknown id takes as long as a wrong secret. */
const NO_CREDENTIAL = "0".repeat(64);

const encoder = new TextEncoder();

/**
 * A new gateway's id, token and the hash of its secret. Only the hash is
 * stored (gateway_credentials); the token is shown once, to the operator.
 */
export async function newGatewayCredential(): Promise<{ gatewayId: string; token: string; secretSha256: string }> {
  const gatewayId = crypto.randomUUID();
  const secret = randomToken(32);
  return { gatewayId, token: `hpgw_${gatewayId}.${secret}`, secretSha256: await sha256Hex(secret) };
}

/**
 * Answers 401 unless the request carries a live gateway token as a Bearer
 * token. The secret's SHA-256 is compared with the stored one in constant
 * time. Sets `gateway` for the handlers.
 */
export const requireGateway = createMiddleware<{ Bindings: Env; Variables: GatewayVariables }>(async (c, next) => {
  const match = GATEWAY_TOKEN.exec(bearerToken(c.req.header("authorization")) ?? "");
  const gatewayId = match?.[1];
  const secret = match?.[2];
  if (gatewayId === undefined || secret === undefined) return unauthorized(c);

  const credential = await c.env.DB.prepare(
    `SELECT secret_sha256 FROM gateway_credentials WHERE gateway_id = ? AND revoked_at IS NULL`,
  )
    .bind(gatewayId)
    .first<{ secret_sha256: string }>();
  const presented = encoder.encode(await sha256Hex(secret));
  const expected = encoder.encode(credential?.secret_sha256 ?? NO_CREDENTIAL);
  // Both are 64 hex characters, so the lengths match.
  const matches = presented.byteLength === expected.byteLength && crypto.subtle.timingSafeEqual(presented, expected);
  if (!credential || !matches) return unauthorized(c);

  c.set("gateway", { id: gatewayId });
  await next();
});
