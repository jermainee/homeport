import Stripe from "stripe";
import { STRIPE_API_VERSION, requireKey } from "./client";

/** One credit pack sale, as Homeport asks Stripe Checkout for it. */
export interface CheckoutSessionInput {
  memberId: string;
  /** pricing.ts CREDIT_PACK_ID. */
  pack: string;
  credits: number;
  /** Net price in integer cents; Stripe Tax adds tax on top. */
  netCents: number;
  /** ISO 4217, lowercase. */
  currency: string;
  /** Stripe product tax code (pricing.ts CREDIT_PACK_TAX_CODE). */
  taxCode: string;
  idempotencyKey: string;
  /** Where Stripe sends the member after paying, and after giving up. */
  successUrl: string;
  cancelUrl: string;
}

/**
 * The calls Homeport makes to Stripe Checkout. Tests replace it with a fake
 * (test/fake-stripe.ts); webhook signatures are checked without it.
 */
export interface StripeCheckoutApi {
  /**
   * Creates a hosted Checkout Session for one credit pack and returns its id
   * and URL. Stripe answers a repeated idempotency key with the first
   * session.
   */
  createCheckoutSession(input: CheckoutSessionInput): Promise<{ id: string; url: string }>;
}

/** Tags Homeport's Checkout Sessions in the Stripe Dashboard. */
const INTEGRATION_IDENTIFIER = "homeport_credit_pack_qzvmrtkw";

/** The Stripe Checkout port backed by Stripe's API. `fetcher` is replaceable for adapter tests. */
export function stripeCheckoutApi(env: Env, fetcher: typeof fetch = fetch): StripeCheckoutApi {
  return {
    async createCheckoutSession(input) {
      const stripe = new Stripe(requireKey(env.STRIPE_SECRET_KEY, "STRIPE_SECRET_KEY"), {
        apiVersion: STRIPE_API_VERSION,
        httpClient: Stripe.createFetchHttpClient(fetcher),
        maxNetworkRetries: 2,
      });
      // The member and pack go on the session, which the webhook reads, and
      // on its PaymentIntent, which refunds and disputes point to.
      const metadata = { member_id: input.memberId, pack: input.pack };
      const session = await stripe.checkout.sessions.create(
        {
          mode: "payment",
          line_items: [
            {
              quantity: 1,
              price_data: {
                currency: input.currency,
                unit_amount: input.netCents,
                tax_behavior: "exclusive",
                product_data: {
                  name: "Homeport credits",
                  description: `${input.credits.toLocaleString("en-US")} credits`,
                  tax_code: input.taxCode,
                },
              },
            },
          ],
          // Stripe Tax adds the tax for the buyer's location. Checkout asks
          // for the address it needs, so no forced billing address. It collects
          // nothing where the account has no active tax registration.
          automatic_tax: { enabled: true },
          client_reference_id: input.memberId,
          metadata,
          payment_intent_data: { metadata },
          success_url: input.successUrl,
          cancel_url: input.cancelUrl,
          integration_identifier: INTEGRATION_IDENTIFIER,
        },
        { idempotencyKey: input.idempotencyKey },
      );
      if (!session.url) throw new Error("Stripe returned a Checkout Session without a URL");
      return { id: session.id, url: session.url };
    },
  };
}
