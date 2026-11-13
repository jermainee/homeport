import { Hono } from "hono";
import Stripe from "stripe";
import { checkoutEventStatements, isCheckoutEvent } from "../checkout/events";
import { apiError } from "../http";
import { identityEventStatements, isIdentityEvent } from "../identity/events";
import type { StripeIdentityApi } from "./identity-api";

const cryptoProvider = Stripe.createSubtleCryptoProvider();

/**
 * Stripe's webhook endpoint. Each event is checked against
 * STRIPE_WEBHOOK_SECRET on the raw body, applied at most once (stripe_events)
 * and answered 200. A failure answers 500, so Stripe delivers it again.
 */
export function stripeWebhook(stripeIdentity: (env: Env) => StripeIdentityApi) {
  const webhook = new Hono<{ Bindings: Env }>();

  webhook.post("/", async (c) => {
    if (!c.env.STRIPE_WEBHOOK_SECRET) throw new Error("STRIPE_WEBHOOK_SECRET is not set");
    const signature = c.req.header("stripe-signature");
    const payload = await c.req.text();
    let event: Stripe.Event;
    try {
      if (!signature) throw new Error("no Stripe-Signature header");
      event = await Stripe.webhooks.constructEventAsync(
        payload,
        signature,
        c.env.STRIPE_WEBHOOK_SECRET,
        undefined,
        cryptoProvider,
      );
    } catch {
      return apiError(c, 400, "invalid_signature");
    }

    const db = c.env.DB;
    const seen = await db.prepare(`SELECT 1 FROM stripe_events WHERE id = ?`).bind(event.id).first();
    if (!seen) {
      const effects = isIdentityEvent(event.type)
        ? await identityEventStatements(db, c.env, stripeIdentity(c.env), event)
        : isCheckoutEvent(event.type)
          ? await checkoutEventStatements(db, event)
          : [];
      await db.batch([
        ...effects,
        db
          .prepare(
            `INSERT INTO stripe_events (id, type, livemode, processed_at) VALUES (?, ?, ?, ?)
             ON CONFLICT (id) DO NOTHING`,
          )
          .bind(event.id, event.type, event.livemode ? 1 : 0, Date.now()),
      ]);
    }
    // Event ids and types only: no member data in logs.
    console.log(JSON.stringify({ event: "stripe_webhook", id: event.id, type: event.type, duplicate: Boolean(seen) }));
    return c.json({ received: true });
  });

  return webhook;
}
