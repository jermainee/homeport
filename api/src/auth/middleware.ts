import type { Context } from "hono";
import { createMiddleware } from "hono/factory";
import { authenticate, type Member } from "./sessions";

export interface SessionVariables {
  member: Member;
  sessionToken: string;
}

/** Rejects the request with 401 unless it carries a live session token as a Bearer token. */
export const requireSession = createMiddleware<{ Bindings: Env; Variables: SessionVariables }>(
  async (c, next) => {
    const token = bearerToken(c.req.header("authorization"));
    const member = token ? await authenticate(c.env.DB, token, Date.now()) : null;
    if (!token || !member) return unauthorized(c);
    c.set("member", member);
    c.set("sessionToken", token);
    await next();
  },
);

/** The token from `Authorization: Bearer <token>`; the scheme is case-insensitive (RFC 7235). */
export function bearerToken(header: string | undefined): string | null {
  const match = /^bearer (\S+)$/i.exec(header ?? "");
  return match?.[1] ?? null;
}

/** 401 with a Bearer challenge, for a missing, malformed, wrong or out-of-scope token. */
export function unauthorized(c: Context): Response {
  c.header("www-authenticate", 'Bearer realm="homeport"');
  return c.json({ error: "unauthorized" }, 401);
}
