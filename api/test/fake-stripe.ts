import { env } from "cloudflare:workers";
import { createApp } from "../src/app";
import type { CheckoutSessionInput, StripeCheckoutApi } from "../src/stripe/checkout-api";
import type { StripeIdentityApi, VerifiedIdentity } from "../src/stripe/identity-api";

/** A Stripe Identity port that records calls and answers from memory, like Stripe's test mode. */
export class FakeStripeIdentity implements StripeIdentityApi {
  readonly created: { memberId: string; idempotencyKey: string }[] = [];
  readonly retrieved: string[] = [];
  /** What `retrieveVerifiedIdentity` returns per session; a default document otherwise. */
  readonly identities = new Map<string, VerifiedIdentity>();
  private readonly byIdempotencyKey = new Map<string, { id: string; url: string }>();

  createVerificationSession(input: { memberId: string; idempotencyKey: string }) {
    this.created.push(input);
    // Stripe answers a repeated idempotency key with the first session.
    const known = this.byIdempotencyKey.get(input.idempotencyKey);
    if (known) return Promise.resolve(known);
    const id = `vs_test_${crypto.randomUUID().replaceAll("-", "")}`;
    const session = { id, url: `https://verify.stripe.com/start/test_${id}` };
    this.byIdempotencyKey.set(input.idempotencyKey, session);
    return Promise.resolve(session);
  }

  /** Set to make the next retrieval fail, as a Stripe outage would. */
  failNextRetrieval = false;

  retrieveVerifiedIdentity(sessionId: string) {
    this.retrieved.push(sessionId);
    if (this.failNextRetrieval) {
      this.failNextRetrieval = false;
      return Promise.reject(new Error("Stripe is unavailable"));
    }
    return Promise.resolve(
      this.identities.get(sessionId) ?? {
        documentNumber: `D${crypto.randomUUID().slice(0, 8)}`,
        issuingCountry: "DE",
        dateOfBirth: { day: 1, month: 2, year: 1990 },
      },
    );
  }
}

/** A Stripe Checkout port that records calls and answers from memory, like Stripe's test mode. */
export class FakeStripeCheckout implements StripeCheckoutApi {
  readonly created: CheckoutSessionInput[] = [];
  private readonly byIdempotencyKey = new Map<string, { id: string; url: string }>();

  /** Set to make the next call fail, as a Stripe outage would. */
  failNextCreate = false;

  createCheckoutSession(input: CheckoutSessionInput) {
    this.created.push(input);
    if (this.failNextCreate) {
      this.failNextCreate = false;
      return Promise.reject(new Error("Stripe is unavailable"));
    }
    // Stripe answers a repeated idempotency key with the first session.
    const known = this.byIdempotencyKey.get(input.idempotencyKey);
    if (known) return Promise.resolve(known);
    const id = `cs_test_${crypto.randomUUID().replaceAll("-", "")}`;
    const session = { id, url: `https://checkout.stripe.com/c/pay/${id}` };
    this.byIdempotencyKey.set(input.idempotencyKey, session);
    return Promise.resolve(session);
  }
}

/** The Worker with a fake Stripe, for `callWith`. */
export function appWith(stripe: FakeStripeIdentity, checkout: FakeStripeCheckout = new FakeStripeCheckout()) {
  return createApp({ stripeIdentity: () => stripe, stripeCheckout: () => checkout });
}

/** A `Stripe-Signature` header for `payload`, computed the way Stripe documents it (HMAC-SHA256 of `t.payload`). */
export async function stripeSignature(
  payload: string,
  secret = env.STRIPE_WEBHOOK_SECRET,
  timestamp = Math.floor(Date.now() / 1000),
): Promise<string> {
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, [
    "sign",
  ]);
  const mac = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(`${timestamp}.${payload}`));
  const hex = Array.from(new Uint8Array(mac), (byte) => byte.toString(16).padStart(2, "0")).join("");
  return `t=${timestamp},v1=${hex}`;
}

export type IdentityEventType = "verified" | "requires_input" | "canceled" | "processing";

/** A Stripe `identity.verification_session.*` event as Stripe sends it (only the fields Homeport reads, plus a few). */
export function identityEvent(
  type: IdentityEventType,
  input: { sessionId: string; memberId: string; livemode?: boolean; created?: number; eventId?: string },
) {
  const created = input.created ?? Math.floor(Date.now() / 1000);
  return {
    id: input.eventId ?? `evt_test_${crypto.randomUUID().replaceAll("-", "")}`,
    object: "event",
    api_version: "2026-10-28.endive",
    created,
    livemode: input.livemode ?? false,
    pending_webhooks: 1,
    request: { id: null, idempotency_key: null },
    type: `identity.verification_session.${type}`,
    data: {
      object: {
        id: input.sessionId,
        object: "identity.verification_session",
        created,
        livemode: input.livemode ?? false,
        metadata: { member_id: input.memberId },
        status: type,
        type: "document",
        last_error: type === "requires_input" ? { code: "document_unverified_other", reason: "The document was not readable." } : null,
      },
    },
  };
}

export type CheckoutEventType = "completed" | "async_payment_succeeded" | "async_payment_failed" | "expired";

/** A Stripe `checkout.session.*` event as Stripe sends it (only the fields Homeport reads, plus a few). */
export function checkoutEvent(
  type: CheckoutEventType,
  input: {
    sessionId: string;
    memberId: string;
    paymentStatus?: "paid" | "unpaid" | "no_payment_required";
    pack?: string;
    livemode?: boolean;
    eventId?: string;
  },
) {
  const created = Math.floor(Date.now() / 1000);
  const paymentStatus =
    input.paymentStatus ?? (type === "completed" || type === "async_payment_succeeded" ? "paid" : "unpaid");
  return {
    id: input.eventId ?? `evt_test_${crypto.randomUUID().replaceAll("-", "")}`,
    object: "event",
    api_version: "2026-10-28.endive",
    created,
    livemode: input.livemode ?? false,
    pending_webhooks: 1,
    request: { id: null, idempotency_key: null },
    type: `checkout.session.${type}`,
    data: {
      object: {
        id: input.sessionId,
        object: "checkout.session",
        mode: "payment",
        created,
        livemode: input.livemode ?? false,
        client_reference_id: input.memberId,
        metadata: { member_id: input.memberId, pack: input.pack ?? "credits_1000000" },
        currency: "eur",
        amount_subtotal: 4000,
        amount_total: 4760,
        payment_status: paymentStatus,
        status: type === "expired" ? "expired" : "complete",
      },
    },
  };
}
