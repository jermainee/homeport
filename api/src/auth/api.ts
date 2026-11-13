import { Hono } from "hono";
import { signInMailer } from "../email";
import { apiError, hasOnlyFields, invalidRequest } from "../http";
import { idempotent, type IdempotentVariables } from "../idempotency";
import { createLoginRequest, pollLoginRequest } from "./login-requests";
import { bearerToken, requireSession, unauthorized, type SessionVariables } from "./middleware";
import { SESSION_TOKEN, revokeSession } from "./sessions";

export const authApi = new Hono<{
  Bindings: Env;
  Variables: SessionVariables & IdempotentVariables;
}>();

const SHA256_HEX = /^[0-9a-f]{64}$/;

authApi.post("/login-requests", idempotent, async (c) => {
  const body = c.get("body");
  if (!hasOnlyFields(body, ["request_id", "email", "session_token_sha256"])) {
    return invalidRequest(c, "unknown field");
  }
  const email = parseEmail(body["email"]);
  if (!email) return invalidRequest(c, "email must be an email address of at most 254 characters");
  const sessionTokenHash = body["session_token_sha256"];
  if (typeof sessionTokenHash !== "string" || !SHA256_HEX.test(sessionTokenHash)) {
    return invalidRequest(c, "session_token_sha256 must be 64 lowercase hex characters");
  }

  const mailer = signInMailer(c.env);
  if (!mailer) return apiError(c, 503, "email_unavailable");

  const request = await createLoginRequest(c.env.DB, { email, sessionTokenHash, now: Date.now() });
  if (!request) return invalidRequest(c, "session_token_sha256 is already in use; create a new token");

  await mailer.send({ to: email, token: request.linkToken, matchCode: request.matchCode });

  return c.json(
    {
      login_request_id: request.id,
      match_code: request.matchCode,
      expires_at: new Date(request.expiresAt).toISOString(),
    },
    201,
  );
});

/**
 * Polling a sign-in. The app sends the token it created as a Bearer token
 * (the contract's `pendingSession`), so requireSession does not guard this
 * route. A poll only reads: no request_id and nothing written.
 */
authApi.get("/login-requests/:id", async (c) => {
  c.header("cache-control", "no-store");
  const sessionToken = bearerToken(c.req.header("authorization"));
  if (!sessionToken || !SESSION_TOKEN.test(sessionToken)) return unauthorized(c);

  const result = await pollLoginRequest(c.env.DB, {
    id: c.req.param("id"),
    sessionToken,
    now: Date.now(),
  });
  switch (result.status) {
    case "not_found":
      return apiError(c, 404, "not_found");
    case "expired":
      return apiError(c, 410, "login_request_expired");
    case "pending":
      return c.json({ status: "pending" }, 202);
    case "approved":
      return c.json({
        status: "approved",
        member_id: result.memberId,
        expires_at: new Date(result.sessionExpiresAt).toISOString(),
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
