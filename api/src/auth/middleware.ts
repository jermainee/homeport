import { createMiddleware } from "hono/factory";
import { authenticate, type Member } from "./sessions";

export interface SessionVariables {
  member: Member;
  sessionToken: string;
}

/** Rejects the request with 401 unless it carries a live session as a Bearer token. */
export const requireSession = createMiddleware<{ Bindings: Env; Variables: SessionVariables }>(
  async (c, next) => {
    const token = bearerToken(c.req.header("authorization"));
    const member = token ? await authenticate(c.env.DB, token, Date.now()) : null;
    if (!token || !member) {
      c.header("www-authenticate", 'Bearer realm="homeport"');
      return c.json({ error: "unauthorized" }, 401);
    }
    c.set("member", member);
    c.set("sessionToken", token);
    await next();
  },
);

function bearerToken(header: string | undefined): string | null {
  const match = /^Bearer ([A-Za-z0-9_-]+)$/.exec(header ?? "");
  return match?.[1] ?? null;
}
