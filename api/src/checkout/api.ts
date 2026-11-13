import { Hono } from "hono";
import { every } from "hono/combine";
import { requireSession, type SessionVariables } from "../auth/middleware";
import { PAGE_HEADERS, messagePage } from "../auth/pages";
import { hasOnlyFields, httpOrigin, invalidRequest } from "../http";
import { type IdempotentVariables, idempotent } from "../idempotency";
import { requireVerifiedIdentity } from "../identity/require-verified";
import { CREDIT_PACK_CREDITS, CREDIT_PACK_CURRENCY, CREDIT_PACK_ID, CREDIT_PACK_NET_CENTS } from "../pricing";
import type { StripeCheckoutApi } from "../stripe/checkout-api";

/**
 * Buying credits (contract operation createCheckoutSession). The member
 * pays on Stripe's hosted page; the credits are granted by the webhook
 * (src/checkout/events.ts), never by the return page.
 */
export function checkoutApi(stripeCheckout: (env: Env) => StripeCheckoutApi) {
  const api = new Hono<{ Bindings: Env; Variables: SessionVariables & IdempotentVariables }>();

  // The identity check runs before idempotent, so a 403 is not stored for the request_id.
  api.post("/sessions", every(requireSession, requireVerifiedIdentity, idempotent), async (c) => {
    if (!hasOnlyFields(c.get("body"), ["request_id"])) return invalidRequest(c, "unknown field");
    const baseUrl = httpOrigin(c.env.PUBLIC_BASE_URL);
    if (!baseUrl) throw new Error("PUBLIC_BASE_URL is not set");
    const memberId = c.get("member").id;
    const requestId = c.get("body")["request_id"] as string;

    const session = await stripeCheckout(c.env).createCheckoutSession({
      memberId,
      pack: CREDIT_PACK_ID,
      credits: CREDIT_PACK_CREDITS,
      netCents: CREDIT_PACK_NET_CENTS,
      currency: CREDIT_PACK_CURRENCY,
      idempotencyKey: `checkout_session:${requestId}`,
      successUrl: new URL("/checkout/done", baseUrl).href,
      cancelUrl: new URL("/checkout/canceled", baseUrl).href,
    });
    const now = Date.now();
    // A retry after a 5xx gets the same session from Stripe, so the row may exist.
    await c.env.DB.prepare(
      `INSERT INTO payments (id, member_id, pack, credits, net_cents, currency, created_at, updated_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
    )
      .bind(session.id, memberId, CREDIT_PACK_ID, CREDIT_PACK_CREDITS, CREDIT_PACK_NET_CENTS, CREDIT_PACK_CURRENCY, now, now)
      .run();

    return c.json({ url: session.url }, 201);
  });

  return api;
}

/** The pages Stripe Checkout returns the member to. They show a message only; the webhook grants the credits. */
export const checkoutPages = new Hono<{ Bindings: Env }>();

checkoutPages.get("/done", (c) =>
  c.html(
    messagePage(
      "Thank you",
      "Your credits appear in the Homeport app as soon as the payment clears, usually within a minute. You can close this page.",
    ),
    200,
    PAGE_HEADERS,
  ),
);

checkoutPages.get("/canceled", (c) =>
  c.html(
    messagePage("No payment made", "You can close this page and buy credits again from the Homeport app."),
    200,
    PAGE_HEADERS,
  ),
);
