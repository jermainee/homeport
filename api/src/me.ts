import { Hono } from "hono";
import { requireSession, type SessionVariables } from "./auth/middleware";

export const meApi = new Hono<{ Bindings: Env; Variables: SessionVariables }>();

meApi.use(requireSession);

meApi.get("/", (c) => {
  const member = c.get("member");
  return c.json({
    id: member.id,
    email: member.email,
    status: member.status,
    identity_status: member.identityStatus,
    created_at: new Date(member.createdAt).toISOString(),
  });
});
