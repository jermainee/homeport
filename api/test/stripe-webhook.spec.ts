import { env } from "cloudflare:workers";
import { beforeEach, describe, expect, it } from "vitest";
import { FakeStripeIdentity, type IdentityEventType, appWith, identityEvent, stripeSignature } from "./fake-stripe";
import { bearer, callWith, signIn, uniqueEmail, uuidv7 } from "./helpers";

type App = ReturnType<typeof appWith>;

async function deliver(app: App, event: unknown, signature?: string): Promise<Response> {
  const payload = JSON.stringify(event);
  return callWith(app, "/v1/stripe/webhook", {
    method: "POST",
    headers: { "content-type": "application/json", "stripe-signature": signature ?? (await stripeSignature(payload)) },
    body: payload,
  });
}

/** A signed-in member who started an identity check; returns the Stripe session id. */
async function memberWithCheck(app: App, stripe: FakeStripeIdentity) {
  const { sessionToken, memberId } = await signIn(uniqueEmail());
  await callWith(app, "/v1/identity/sessions", {
    method: "POST",
    headers: { "content-type": "application/json", authorization: `Bearer ${sessionToken}` },
    body: JSON.stringify({ request_id: uuidv7() }),
  });
  const sessionId = (await sessionIdsOf(memberId)).at(-1);
  if (!stripe.created.some((call) => call.memberId === memberId) || !sessionId) {
    throw new Error("no identity session created");
  }
  return { sessionToken, memberId, sessionId };
}

async function sessionIdsOf(memberId: string): Promise<string[]> {
  const { results } = await env.DB.prepare(`SELECT id FROM identity_sessions WHERE member_id = ? ORDER BY created_at`)
    .bind(memberId)
    .all<{ id: string }>();
  return results.map((row) => row.id);
}

async function identityStatus(sessionToken: string, app: App): Promise<string> {
  const me = await (await callWith(app, "/v1/me", bearer(sessionToken))).json<{ identity_status: string }>();
  return me.identity_status;
}

async function storedHash(memberId: string): Promise<string | null> {
  const row = await env.DB.prepare(`SELECT identity_hash FROM members WHERE id = ?`).bind(memberId).first<{ identity_hash: string | null }>();
  return row?.identity_hash ?? null;
}

describe("POST /v1/stripe/webhook: signature", () => {
  it("rejects an event without a valid signature, and changes nothing", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const { sessionToken, memberId, sessionId } = await memberWithCheck(app, stripe);
    const event = identityEvent("verified", { sessionId, memberId });
    const payload = JSON.stringify(event);

    const forged = await deliver(app, event, await stripeSignature(payload, "whsec_someone_else"));
    const missing = await callWith(app, "/v1/stripe/webhook", { method: "POST", body: payload });
    const tampered = await deliver(app, { ...event, livemode: true }, await stripeSignature(payload));

    expect([forged.status, missing.status, tampered.status]).toEqual([400, 400, 400]);
    expect(await forged.json()).toEqual({ error: "invalid_signature" });
    expect(await identityStatus(sessionToken, app)).toBe("unverified");
  });

  it("rejects a signature older than Stripe's five-minute tolerance", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const { memberId, sessionId } = await memberWithCheck(app, stripe);
    const event = identityEvent("verified", { sessionId, memberId });
    const payload = JSON.stringify(event);

    const response = await deliver(app, event, await stripeSignature(payload, undefined, Math.floor(Date.now() / 1000) - 600));

    expect(response.status).toBe(400);
  });
});

describe("POST /v1/stripe/webhook: identity events", () => {
  it("verifies the member on identity.verification_session.verified", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const { sessionToken, memberId, sessionId } = await memberWithCheck(app, stripe);

    const response = await deliver(app, identityEvent("verified", { sessionId, memberId, livemode: true }));

    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ received: true });
    expect(await identityStatus(sessionToken, app)).toBe("verified");
    expect(stripe.retrieved).toEqual([sessionId]);
    expect(await storedHash(memberId)).toMatch(/^[0-9a-f]{64}$/);
  });

  it.each([
    ["requires_input", "requires_input"],
    ["canceled", "requires_input"],
    ["processing", "processing"],
  ] as [IdentityEventType, string][])("maps %s to identity_status %s", async (type, status) => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const { sessionToken, memberId, sessionId } = await memberWithCheck(app, stripe);

    const response = await deliver(app, identityEvent(type, { sessionId, memberId }));

    expect(response.status).toBe(200);
    expect(await identityStatus(sessionToken, app)).toBe(status);
  });

  it("applies an event delivered twice only once", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const { memberId, sessionId } = await memberWithCheck(app, stripe);
    const event = identityEvent("verified", { sessionId, memberId, livemode: true });

    await deliver(app, event);
    const again = await deliver(app, event);

    expect(again.status).toBe(200);
    expect(stripe.retrieved).toEqual([sessionId]);
  });

  it("ignores an older event that arrives after a newer one", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const { sessionToken, memberId, sessionId } = await memberWithCheck(app, stripe);
    const now = Math.floor(Date.now() / 1000);

    await deliver(app, identityEvent("processing", { sessionId, memberId, created: now }));
    await deliver(app, identityEvent("requires_input", { sessionId, memberId, created: now - 60 }));

    expect(await identityStatus(sessionToken, app)).toBe("processing");
  });

  it("never takes a verification back", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const { sessionToken, memberId, sessionId } = await memberWithCheck(app, stripe);
    const now = Math.floor(Date.now() / 1000);
    await deliver(app, identityEvent("verified", { sessionId, memberId, created: now }));

    await deliver(app, identityEvent("canceled", { sessionId, memberId, created: now + 60 }));

    expect(await identityStatus(sessionToken, app)).toBe("verified");
  });

  it("ignores events of sessions Homeport did not create for that member", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const victim = await memberWithCheck(app, stripe);
    const other = await memberWithCheck(app, stripe);

    // Another member's session, with metadata pointing at the victim.
    const response = await deliver(app, identityEvent("verified", { sessionId: other.sessionId, memberId: victim.memberId }));
    const unknown = await deliver(app, identityEvent("verified", { sessionId: "vs_unknown", memberId: victim.memberId }));

    expect([response.status, unknown.status]).toEqual([200, 200]);
    expect(await identityStatus(victim.sessionToken, app)).toBe("unverified");
    expect(await identityStatus(other.sessionToken, app)).toBe("unverified");
  });

  it("accepts events larger than the 16 KB limit of member endpoints", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const { sessionToken, memberId, sessionId } = await memberWithCheck(app, stripe);
    const event = identityEvent("processing", { sessionId, memberId });
    const large = { ...event, data: { object: { ...event.data.object, padding: "x".repeat(40_000) } } };

    const response = await deliver(app, large);

    expect(response.status).toBe(200);
    expect(await identityStatus(sessionToken, app)).toBe("processing");
  });

  it("acknowledges event types it does not handle", async () => {
    const response = await deliver(appWith(new FakeStripeIdentity()), {
      id: `evt_test_${crypto.randomUUID()}`,
      object: "event",
      type: "customer.created",
      created: Math.floor(Date.now() / 1000),
      livemode: false,
      data: { object: { id: "cus_123", object: "customer" } },
    });

    expect(response.status).toBe(200);
  });
});

describe("one person, one account", () => {
  // The database outlives a test, so each test gets its own document number.
  let document: { documentNumber: string; issuingCountry: string; dateOfBirth: { day: number; month: number; year: number } };
  beforeEach(() => {
    const number = `C01X${crypto.randomUUID().slice(0, 5).toUpperCase()}`;
    document = { documentNumber: number, issuingCountry: "DE", dateOfBirth: { day: 12, month: 8, year: 1964 } };
  });

  it("does not verify a second member with an identity another member verified (live mode)", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const first = await memberWithCheck(app, stripe);
    const second = await memberWithCheck(app, stripe);
    stripe.identities.set(first.sessionId, document);
    // Same document, written differently.
    stripe.identities.set(second.sessionId, {
      ...document,
      documentNumber: `${document.documentNumber.slice(0, 4).toLowerCase()} ${document.documentNumber.slice(4).toLowerCase()}`,
    });

    await deliver(app, identityEvent("verified", { sessionId: first.sessionId, memberId: first.memberId, livemode: true }));
    const response = await deliver(app, identityEvent("verified", { sessionId: second.sessionId, memberId: second.memberId, livemode: true }));

    expect(response.status).toBe(200);
    expect(await identityStatus(first.sessionToken, app)).toBe("verified");
    expect(await identityStatus(second.sessionToken, app)).toBe("requires_input");
    expect(await storedHash(second.memberId)).toBeNull();
  });

  it("tells apart identities that differ in any of country, number or birth date", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const members = [];
    // One at a time: signIn reads the dev email log through a console spy.
    for (let i = 0; i < 4; i++) members.push(await memberWithCheck(app, stripe));
    const variants = [
      document,
      { ...document, issuingCountry: "AT" },
      { ...document, documentNumber: `${document.documentNumber}8` },
      { ...document, dateOfBirth: { day: 13, month: 8, year: 1964 } },
    ];
    members.forEach((member, index) => stripe.identities.set(member.sessionId, variants[index] ?? document));

    for (const member of members) {
      await deliver(app, identityEvent("verified", { sessionId: member.sessionId, memberId: member.memberId, livemode: true }));
    }

    for (const member of members) expect(await identityStatus(member.sessionToken, app)).toBe("verified");
  });

  it("skips the check in test mode, where every test document is the same", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const first = await memberWithCheck(app, stripe);
    const second = await memberWithCheck(app, stripe);
    stripe.identities.set(first.sessionId, document);
    stripe.identities.set(second.sessionId, document);

    await deliver(app, identityEvent("verified", { sessionId: first.sessionId, memberId: first.memberId, livemode: false }));
    await deliver(app, identityEvent("verified", { sessionId: second.sessionId, memberId: second.memberId, livemode: false }));

    expect(await identityStatus(first.sessionToken, app)).toBe("verified");
    expect(await identityStatus(second.sessionToken, app)).toBe("verified");
    expect(await storedHash(first.memberId)).toBeNull();
  });

  it("does not verify a live-mode member whose document lacks the hashed fields", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const member = await memberWithCheck(app, stripe);
    stripe.identities.set(member.sessionId, { ...document, documentNumber: null });

    await deliver(app, identityEvent("verified", { sessionId: member.sessionId, memberId: member.memberId, livemode: true }));

    expect(await identityStatus(member.sessionToken, app)).toBe("requires_input");
  });
});
