import { Hono } from "hono";
import { sendSignInEmail } from "../email";
import { readJsonObject } from "../http";
import { createLoginRequest, pollLoginRequest } from "./login-requests";
import { requireSession, type SessionVariables } from "./middleware";
import { revokeSession } from "./sessions";

export const authApi = new Hono<{ Bindings: Env; Variables: SessionVariables }>();

authApi.post("/login-requests", async (c) => {
  const body = await readJsonObject(c.req.raw);
  const email = parseEmail(body?.["email"]);
  if (!email) return c.json({ error: "invalid_email" }, 400);
  const installId = parseInstallId(body?.["install_id"]);
  if (installId === undefined) return c.json({ error: "invalid_install_id" }, 400);

  const request = await createLoginRequest(c.env.DB, { email, installId, now: Date.now() });

  await sendSignInEmail(c.env, { to: email, token: request.linkToken, matchCode: request.matchCode });

  return c.json(
    {
      login_request_id: request.id,
      poll_secret: request.pollSecret,
      match_code: request.matchCode,
      expires_at: new Date(request.expiresAt).toISOString(),
    },
    201,
  );
});

authApi.post("/login-requests/:id/poll", async (c) => {
  const body = await readJsonObject(c.req.raw);
  const pollSecret = body?.["poll_secret"];
  if (typeof pollSecret !== "string") return c.json({ error: "invalid_poll_secret" }, 400);

  const result = await pollLoginRequest(c.env.DB, {
    id: c.req.param("id"),
    pollSecret,
    now: Date.now(),
  });
  switch (result.status) {
    case "not_found":
      return c.json({ error: "not_found" }, 404);
    case "expired":
      return c.json({ error: "expired" }, 410);
    case "already_collected":
      return c.json({ error: "already_collected" }, 410);
    case "pending":
      return c.json({ status: "pending" }, 202);
    case "confirmed":
      return c.json({
        status: "confirmed",
        session_token: result.sessionToken,
        expires_at: new Date(result.expiresAt).toISOString(),
      });
  }
});

authApi.delete("/session", requireSession, async (c) => {
  await revokeSession(c.env.DB, c.get("sessionToken"), Date.now());
  return c.body(null, 204);
});

function parseEmail(value: unknown): string | null {
  if (typeof value !== "string") return null;
  const email = value.trim().toLowerCase();
  if (email.length > 254 || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) return null;
  return email;
}

/** The optional id the app picks once per install: null when absent, undefined when invalid. */
function parseInstallId(value: unknown): string | null | undefined {
  if (value === undefined || value === null) return null;
  if (typeof value !== "string" || !/^[A-Za-z0-9_-]{1,64}$/.test(value)) return undefined;
  return value;
}
