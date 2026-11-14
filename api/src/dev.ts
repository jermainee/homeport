import { Hono } from "hono";
import { approveWithSessionToken } from "./auth/login-requests";
import { parseGatewaySelf, storeGatewaySelf } from "./gateway/self";
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
 * Stores a gateway's announcement under any id, without a gateway token, so
 * lines and WireGuard configs can be created in local runs and tests. A
 * real gateway sends PUT /v1/gateway/self. The body is the contract's
 * GatewaySelf.
 */
devRoutes.put("/gateways/:id", async (c) => {
  const self = parseGatewaySelf(await readJsonObject(c.req.raw));
  if (!self) return invalidRequest(c, "body must be the contract's GatewaySelf");
  await storeGatewaySelf(c.env.DB, c.req.param("id"), self, Date.now()).run();
  return c.body(null, 204);
});

/**
 * Marks a member's identity verified, as Stripe Identity's `verified`
 * webhook does in test mode (no identity hash), so local runs and
 * end-to-end tests can share and use lines without an Identity check.
 */
devRoutes.post("/members/:id/identity/verify", async (c) => {
  const verified = await c.env.DB.prepare(
    `UPDATE members SET identity_status = 'verified', identity_failure = NULL WHERE id = ? RETURNING id`,
  )
    .bind(c.req.param("id"))
    .first();
  if (!verified) return apiError(c, 404, "not_found");
  return c.json({ identity_status: "verified" });
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
