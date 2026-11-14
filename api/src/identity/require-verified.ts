import { every } from "hono/combine";
import { createMiddleware } from "hono/factory";
import { requireSession, type SessionVariables } from "../auth/middleware";
import { apiError } from "../http";

/**
 * Answers 403 identity_required unless the signed-in member passed Stripe
 * Identity. Share and Use stay off until then: devices, lines, WireGuard
 * peers and credit purchases sit behind this gate. Runs after requireSession.
 */
export const requireVerifiedIdentity = createMiddleware<{ Bindings: Env; Variables: SessionVariables }>(
  async (c, next) => {
    if (c.get("member").identityStatus !== "verified") return apiError(c, 403, "identity_required");
    await next();
  },
);

/** A signed-in member who passed Stripe Identity: requireSession, then requireVerifiedIdentity. */
export const requireVerifiedMember = every(requireSession, requireVerifiedIdentity);
