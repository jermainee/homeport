import { Hono } from "hono";
import { requireSession, type SessionVariables } from "../auth/middleware";
import { apiError, hasOnlyFields, invalidRequest } from "../http";
import { idempotent, type IdempotentVariables } from "../idempotency";
import type { StripeIdentityApi } from "../stripe/identity-api";

/** Member endpoints for the Stripe Identity check (contract operation createIdentitySession). */
export function identityApi(stripeIdentity: (env: Env) => StripeIdentityApi) {
  const api = new Hono<{ Bindings: Env; Variables: SessionVariables & IdempotentVariables }>();

  api.post("/sessions", requireSession, idempotent, async (c) => {
    if (!hasOnlyFields(c.get("body"), ["request_id"])) return invalidRequest(c, "unknown field");
    const member = c.get("member");
    if (member.identityStatus === "verified") return apiError(c, 409, "identity_already_verified");

    const requestId = c.get("body")["request_id"] as string;
    const session = await stripeIdentity(c.env).createVerificationSession({
      memberId: member.id,
      idempotencyKey: `identity_session:${requestId}`,
    });
    await c.env.DB.prepare(
      `INSERT INTO identity_sessions (id, member_id, created_at) VALUES (?, ?, ?)
       ON CONFLICT (id) DO NOTHING`,
    )
      .bind(session.id, member.id, Date.now())
      .run();

    return c.json({ url: session.url }, 201);
  });

  return api;
}
