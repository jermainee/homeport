import { Hono } from "hono";
import { approveWithSessionToken } from "./auth/login-requests";
import { apiError, hasOnlyFields, invalidRequest, readJsonObject } from "./http";
import { getBalances, grantCredits } from "./ledger";

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

/**
 * Grants a member purchased credits from the dev_grants account, so local
 * runs and end-to-end tests can use lines before credit packs can be bought.
 */
devRoutes.post("/members/:id/credits", async (c) => {
  const body = await readJsonObject(c.req.raw);
  const credits = body?.["credits"];
  if (!body || !hasOnlyFields(body, ["credits"]) || typeof credits !== "number" || !Number.isSafeInteger(credits) || credits <= 0) {
    return invalidRequest(c, "credits must be a positive whole number");
  }
  const memberId = c.req.param("id");
  if (!(await getBalances(c.env.DB, memberId))) return apiError(c, 404, "not_found");

  await grantCredits(c.env.DB, {
    memberId,
    credits,
    source: "dev_grants",
    idempotencyKey: `dev_grant:${crypto.randomUUID()}`,
  });
  return c.json({ credits: await getBalances(c.env.DB, memberId) });
});
