import { describe, expect, it } from "vitest";
import pkg from "../package.json";
import { isCheckoutEvent } from "../src/checkout/events";
import { isIdentityEvent } from "../src/identity/events";

// Every event type Stripe sends for Checkout Sessions and Identity
// VerificationSessions, handled or not.
const STRIPE_EVENT_TYPES = [
  "checkout.session.async_payment_failed",
  "checkout.session.async_payment_succeeded",
  "checkout.session.completed",
  "checkout.session.expired",
  "identity.verification_session.canceled",
  "identity.verification_session.created",
  "identity.verification_session.processing",
  "identity.verification_session.redacted",
  "identity.verification_session.requires_input",
  "identity.verification_session.verified",
];

const scripts: Record<string, string | undefined> = pkg.scripts;

describe("npm run stripe:listen", () => {
  const script = scripts["stripe:listen"] ?? "";

  it("forwards Stripe's events to the local webhook route", () => {
    expect(script).toMatch(/^stripe listen .*--forward-to localhost:8787\/v1\/stripe\/webhook( |$)/);
  });

  it("forwards exactly the events the webhook handles", () => {
    const forwarded = (/--events (\S+)/.exec(script)?.[1] ?? "").split(",").filter(Boolean).sort();
    const handled = STRIPE_EVENT_TYPES.filter((type) => isIdentityEvent(type) || isCheckoutEvent(type));
    expect(forwarded).toEqual(handled);
  });
});
