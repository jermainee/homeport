import { Hono } from "hono";
import { confirmWithPollSecret } from "./auth/login-requests";
import { readJsonObject } from "./http";

/**
 * Shortcuts for local development and automated tests. Every route here
 * answers 404 unless ENVIRONMENT is exactly "dev".
 */
export const devRoutes = new Hono<{ Bindings: Env }>();

devRoutes.use(async (c, next) => {
  if (c.env.ENVIRONMENT !== "dev") return c.json({ error: "not_found" }, 404);
  await next();
});

devRoutes.post("/login-requests/:id/confirm", async (c) => {
  const body = await readJsonObject(c.req.raw);
  const pollSecret = body?.["poll_secret"];
  if (typeof pollSecret !== "string") return c.json({ error: "invalid_poll_secret" }, 400);

  const result = await confirmWithPollSecret(c.env.DB, {
    id: c.req.param("id"),
    pollSecret,
    now: Date.now(),
  });
  switch (result) {
    case "not_found":
      return c.json({ error: "not_found" }, 404);
    case "expired":
      return c.json({ error: "expired" }, 410);
    case "confirmed":
      return c.json({ status: "confirmed" });
  }
});
