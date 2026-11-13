import { Hono } from "hono";
import type { SessionVariables } from "../auth/middleware";
import { apiError, hasOnlyFields, invalidRequest } from "../http";
import { type IdempotentVariables, memberIdempotent } from "../idempotency";
import type { StripeIdentityApi } from "../stripe/identity-api";

/** Checks a member may start per CHECK_WINDOW_MS. Stripe bills each submitted check. */
export const MAX_CHECKS_PER_WINDOW = 5;
export const CHECK_WINDOW_MS = 24 * 60 * 60 * 1000;

/** Member endpoints for the Stripe Identity check (contract operation createIdentitySession). */
export function identityApi(stripeIdentity: (env: Env) => StripeIdentityApi) {
  const api = new Hono<{ Bindings: Env; Variables: SessionVariables & IdempotentVariables }>();

  api.post("/sessions", memberIdempotent, async (c) => {
    if (!hasOnlyFields(c.get("body"), ["request_id"])) return invalidRequest(c, "unknown field");
    const member = c.get("member");
    if (member.identityStatus === "verified") return apiError(c, 409, "identity_already_verified");

    const now = Date.now();
    const recent = await c.env.DB.prepare(
      `SELECT COUNT(*) AS count, MIN(created_at) AS oldest FROM identity_sessions
        WHERE member_id = ? AND created_at > ?`,
    )
      .bind(member.id, now - CHECK_WINDOW_MS)
      .first<{ count: number; oldest: number | null }>();
    if (recent && recent.count >= MAX_CHECKS_PER_WINDOW) {
      const freeAt = (recent.oldest ?? now) + CHECK_WINDOW_MS;
      c.header("retry-after", String(Math.max(1, Math.ceil((freeAt - now) / 1000))));
      return apiError(c, 429, "rate_limited", `at most ${MAX_CHECKS_PER_WINDOW} identity checks a day`);
    }

    const requestId = c.get("body")["request_id"] as string;
    const session = await stripeIdentity(c.env).createVerificationSession({
      memberId: member.id,
      idempotencyKey: `identity_session:${requestId}`,
    });
    await c.env.DB.prepare(
      `INSERT INTO identity_sessions (id, member_id, created_at) VALUES (?, ?, ?)
       ON CONFLICT (id) DO NOTHING`,
    )
      .bind(session.id, member.id, now)
      .run();

    return c.json({ url: session.url }, 201);
  });

  return api;
}
