import { Hono } from "hono";
import { approveWithSessionToken } from "./auth/login-requests";
import { apiError, invalidRequest, readJsonObject } from "./http";

/**
 * Shortcuts for local development and automated tests. Every route here
 * answers 404 unless ENVIRONMENT is exactly "dev".
 */
export const devRoutes = new Hono<{ Bindings: Env }>();

devRoutes.use(async (c, next) => {
  if (c.env.ENVIRONMENT !== "dev") return apiError(c, 404, "not_found");
  await next();
});

/** Approves a login request as if the member had confirmed the emailed link. */
devRoutes.post("/login-requests/:id/confirm", async (c) => {
  const body = await readJsonObject(c.req.raw);
  const sessionToken = body?.["session_token"];
  if (typeof sessionToken !== "string") return invalidRequest(c, "session_token is required");

  const result = await approveWithSessionToken(c.env.DB, {
    id: c.req.param("id"),
    sessionToken,
    now: Date.now(),
  });
  switch (result) {
    case "not_found":
      return apiError(c, 404, "not_found");
    case "expired":
      return apiError(c, 410, "login_request_expired");
    case "approved":
      return c.json({ status: "approved" });
  }
});
