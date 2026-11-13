import { Hono } from "hono";
import { routePath } from "hono/route";
import { authApi } from "./auth/api";
import { confirmPages } from "./auth/confirm";
import { checkoutApi, checkoutPages } from "./checkout/api";
import { limitBodySize } from "./body-limits";
import { devRoutes } from "./dev";
import { devicesApi } from "./devices";
import { gatewayApi } from "./gateway/api";
import { identityApi } from "./identity/api";
import { linesApi } from "./lines/api";
import { meApi } from "./me";
import { type StripeCheckoutApi, stripeCheckoutApi } from "./stripe/checkout-api";
import { type StripeIdentityApi, stripeIdentityApi } from "./stripe/identity-api";
import { stripeWebhook } from "./stripe/webhook";
import { wireguardApi } from "./wireguard/api";

/** What the Worker talks to outside Cloudflare. Tests pass fakes. */
export interface Dependencies {
  stripeIdentity: (env: Env) => StripeIdentityApi;
  stripeCheckout: (env: Env) => StripeCheckoutApi;
}

export const productionDependencies: Dependencies = {
  stripeIdentity: (env) => stripeIdentityApi(env),
  stripeCheckout: (env) => stripeCheckoutApi(env),
};

export function createApp(dependencies: Dependencies = productionDependencies) {
  const app = new Hono<{ Bindings: Env }>();

  app.use(limitBodySize);

  app.route("/v1/gateway", gatewayApi);
  app.route("/v1/auth", authApi);
  app.route("/v1/me", meApi);
  app.route("/v1/identity", identityApi(dependencies.stripeIdentity));
  app.route("/v1/checkout", checkoutApi(dependencies.stripeCheckout));
  app.route("/v1/devices", devicesApi);
  app.route("/v1/lines", linesApi);
  app.route("/v1/wireguard", wireguardApi);
  app.route("/v1/stripe/webhook", stripeWebhook(dependencies.stripeIdentity));
  app.route("/auth", confirmPages);
  app.route("/checkout", checkoutPages);
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
