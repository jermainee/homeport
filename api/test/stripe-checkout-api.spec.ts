import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { stripeCheckoutApi } from "../src/stripe/checkout-api";

interface Recorded {
  method: string;
  url: URL;
  headers: Headers;
  body: string;
}

/** A fetch that records each request and answers with `reply`'s JSON, as Stripe's API would. */
function recordingFetch(reply: (request: Recorded) => unknown) {
  const requests: Recorded[] = [];
  const fetcher = async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init);
    requests.push({ method: request.method, url: new URL(request.url), headers: request.headers, body: await request.text() });
    return Response.json(reply(requests.at(-1) as Recorded), { headers: { "request-id": "req_test" } });
  };
  return { fetcher, requests };
}

const keys = { ...env, STRIPE_SECRET_KEY: "rk_test_checkout" };

const input = {
  memberId: "01a1c4af-8795-7a28-9249-adbd2ba1c04a",
  pack: "credits_1000000",
  credits: 1_000_000,
  netCents: 4_000,
  currency: "eur",
  idempotencyKey: "checkout_session:01a1d422-ae08-7094-9ddf-d64adb4fed8d",
  successUrl: "https://api.homeport.example/checkout/done",
  cancelUrl: "https://api.homeport.example/checkout/canceled",
};

describe("Stripe Checkout adapter", () => {
  it("creates a one-time payment for one credit pack at its net price, tagged with the member", async () => {
    const { fetcher, requests } = recordingFetch(() => ({
      id: "cs_test_123",
      object: "checkout.session",
      url: "https://checkout.stripe.com/c/pay/cs_test_123",
    }));

    const session = await stripeCheckoutApi(keys, fetcher).createCheckoutSession(input);

    expect(session).toEqual({ id: "cs_test_123", url: "https://checkout.stripe.com/c/pay/cs_test_123" });
    const [request] = requests;
    expect(request?.method).toBe("POST");
    expect(request?.url.pathname).toBe("/v1/checkout/sessions");
    expect(request?.headers.get("authorization")).toBe("Bearer rk_test_checkout");
    expect(request?.headers.get("idempotency-key")).toBe(input.idempotencyKey);
    expect(request?.headers.get("stripe-version")).toBe("2026-10-28.endive");
    const params = Object.fromEntries(new URLSearchParams(request?.body));
    expect(params).toEqual({
      mode: "payment",
      "line_items[0][quantity]": "1",
      "line_items[0][price_data][currency]": "eur",
      "line_items[0][price_data][unit_amount]": "4000",
      // VAT is added on top of the net price.
      "line_items[0][price_data][tax_behavior]": "exclusive",
      "line_items[0][price_data][product_data][name]": "Homeport credits",
      "line_items[0][price_data][product_data][description]": "1,000,000 credits",
      client_reference_id: input.memberId,
      "metadata[member_id]": input.memberId,
      "metadata[pack]": "credits_1000000",
      "payment_intent_data[metadata][member_id]": input.memberId,
      "payment_intent_data[metadata][pack]": "credits_1000000",
      success_url: input.successUrl,
      cancel_url: input.cancelUrl,
      integration_identifier: "homeport_credit_pack_qzvmrtkw",
    });
    // Dynamic payment methods: the Dashboard decides which methods appear.
    expect(Object.keys(params).some((key) => key.startsWith("payment_method_types"))).toBe(false);
  });

  it("refuses to call Stripe without its key", async () => {
    const { fetcher, requests } = recordingFetch(() => ({}));

    await expect(stripeCheckoutApi({ ...keys, STRIPE_SECRET_KEY: "" }, fetcher).createCheckoutSession(input)).rejects.toThrow(
      /STRIPE_SECRET_KEY/,
    );
    expect(requests).toEqual([]);
  });

  it("fails when Stripe returns a session without a URL", async () => {
    const { fetcher } = recordingFetch(() => ({ id: "cs_test_123", object: "checkout.session", url: null }));

    await expect(stripeCheckoutApi(keys, fetcher).createCheckoutSession(input)).rejects.toThrow(/URL/);
  });
});
