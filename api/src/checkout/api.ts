import { Hono } from "hono";
import type { SessionVariables } from "../auth/middleware";
import { PAGE_HEADERS, messagePage } from "../auth/pages";
import { apiError, hasOnlyFields, httpOrigin, invalidRequest } from "../http";
import { type IdempotentVariables, verifiedMemberIdempotent } from "../idempotency";
import {
  CHECKOUT_SESSION_WINDOW_MS,
  CREDIT_PACK_CREDITS,
  CREDIT_PACK_CURRENCY,
  CREDIT_PACK_ID,
  CREDIT_PACK_NET_CENTS,
  CREDIT_PACK_TAX_CODE,
  MAX_CHECKOUT_SESSIONS_PER_WINDOW,
} from "../pricing";
import type { StripeCheckoutApi } from "../stripe/checkout-api";

/**
 * Buying credits (contract operation createCheckoutSession). The member
 * pays on Stripe's hosted page; the credits are granted by the webhook
 * (src/checkout/events.ts), never by the return page.
 */
export function checkoutApi(stripeCheckout: (env: Env) => StripeCheckoutApi) {
  const api = new Hono<{ Bindings: Env; Variables: SessionVariables & IdempotentVariables }>();

  api.post("/sessions", verifiedMemberIdempotent, async (c) => {
    if (!hasOnlyFields(c.get("body"), ["request_id"])) return invalidRequest(c, "unknown field");
    const baseUrl = httpOrigin(c.env.PUBLIC_BASE_URL);
    if (!baseUrl) throw new Error("PUBLIC_BASE_URL is not set");
    const memberId = c.get("member").id;
    const requestId = c.get("body")["request_id"] as string;

    const now = Date.now();
    const recent = await c.env.DB.prepare(
      `SELECT COUNT(*) AS count, MIN(created_at) AS oldest FROM payments WHERE member_id = ? AND created_at > ?`,
    )
      .bind(memberId, now - CHECKOUT_SESSION_WINDOW_MS)
      .first<{ count: number; oldest: number | null }>();
    if (recent && recent.count >= MAX_CHECKOUT_SESSIONS_PER_WINDOW) {
      const freeAt = (recent.oldest ?? now) + CHECKOUT_SESSION_WINDOW_MS;
      c.header("retry-after", String(Math.max(1, Math.ceil((freeAt - now) / 1000))));
      return apiError(c, 429, "rate_limited", `at most ${MAX_CHECKOUT_SESSIONS_PER_WINDOW} checkouts in ${CHECKOUT_SESSION_WINDOW_MS / 60_000} minutes`);
    }

    const session = await stripeCheckout(c.env).createCheckoutSession({
      memberId,
      pack: CREDIT_PACK_ID,
      credits: CREDIT_PACK_CREDITS,
      netCents: CREDIT_PACK_NET_CENTS,
      currency: CREDIT_PACK_CURRENCY,
      taxCode: CREDIT_PACK_TAX_CODE,
      idempotencyKey: `checkout_session:${requestId}`,
      successUrl: new URL("/checkout/done", baseUrl).href,
      cancelUrl: new URL("/checkout/canceled", baseUrl).href,
    });
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
      "Your credits appear in the Homeport app once the payment clears: within a minute for cards, after a few days for bank debits. You can close this page.",
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
