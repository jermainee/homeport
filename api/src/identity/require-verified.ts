import { createMiddleware } from "hono/factory";
import type { SessionVariables } from "../auth/middleware";
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
