import { Hono } from "hono";
import { routePath } from "hono/route";
import { authApi } from "./auth/api";
import { confirmPages } from "./auth/confirm";
import { limitBodySize } from "./body-limits";
import { devRoutes } from "./dev";
import { identityApi } from "./identity/api";
import { meApi } from "./me";
import { type StripeIdentityApi, stripeIdentityApi } from "./stripe/identity-api";
import { stripeWebhook } from "./stripe/webhook";

/** What the Worker talks to outside Cloudflare. Tests pass fakes. */
export interface Dependencies {
  stripeIdentity: (env: Env) => StripeIdentityApi;
}

export const productionDependencies: Dependencies = {
  stripeIdentity: (env) => stripeIdentityApi(env),
};

export function createApp(dependencies: Dependencies = productionDependencies) {
  const app = new Hono<{ Bindings: Env }>();

  app.use(limitBodySize);

  app.route("/v1/auth", authApi);
  app.route("/v1/me", meApi);
  app.route("/v1/identity", identityApi(dependencies.stripeIdentity));
  app.route("/v1/stripe/webhook", stripeWebhook(dependencies.stripeIdentity));
  app.route("/auth", confirmPages);
  app.route("/dev", devRoutes);

  app.notFound((c) => c.json({ error: "not_found" }, 404));

  app.onError((error, c) => {
    // Log only what went wrong and where, never request bodies or headers.
    console.error(
      JSON.stringify({
        event: "unhandled_error",
        method: c.req.method,
        route: routePath(c),
        error: error.name,
        message: error.message,
      }),
    );
    return c.json({ error: "internal_error" }, 500);
  });

  return app;
}
