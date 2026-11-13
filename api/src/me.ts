import { Hono } from "hono";
import { requireSession, type SessionVariables } from "./auth/middleware";

export const meApi = new Hono<{ Bindings: Env; Variables: SessionVariables }>();

meApi.use(requireSession);

meApi.get("/", (c) => {
  const member = c.get("member");
  return c.json({
    member_id: member.id,
    email: member.email,
    identity_status: member.identityStatus,
    // The credits ledger arrives in a later PR; until then no member has credits.
    credits: { purchased: 0, earned_pending: 0, earned_cashable: 0 },
  });
});
