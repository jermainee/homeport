import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { type CheckoutEventType, FakeStripeCheckout, FakeStripeIdentity, appWith, checkoutEvent, stripeSignature } from "./fake-stripe";
import { bearer, callWith, uuidv7 } from "./helpers";
import { driftedBalances, platformBalance, unbalancedTransactions } from "./ledger-fixtures";
import { verifiedMember } from "./share-fixtures";

type App = ReturnType<typeof appWith>;

function setup() {
  const checkout = new FakeStripeCheckout();
  return { checkout, app: appWith(new FakeStripeIdentity(), checkout) };
}

async function deliver(app: App, event: unknown, envOverrides: Partial<Env> = {}): Promise<Response> {
  const payload = JSON.stringify(event);
  return callWith(
    app,
    "/v1/stripe/webhook",
    {
      method: "POST",
      headers: { "content-type": "application/json", "stripe-signature": await stripeSignature(payload) },
      body: payload,
    },
    envOverrides,
  );
}

/** The test database, except that its `failing`-th `batch` call throws, as a D1 outage would. */
function dbFailingBatch(failing: number): D1Database {
  let batches = 0;
  return new Proxy(env.DB, {
    get(target, property) {
      if (property === "batch") {
        return (statements: D1PreparedStatement[]) => {
          batches += 1;
          if (batches === failing) return Promise.reject(new Error("D1 is unavailable"));
          return target.batch(statements);
        };
      }
      const value: unknown = Reflect.get(target, property);
      return typeof value === "function" ? (value as (...args: unknown[]) => unknown).bind(target) : value;
    },
  });
}

/** A verified member who opened a Checkout session; returns its Stripe session id. */
async function buyer(app: App) {
  const { sessionToken, memberId } = await verifiedMember();
  const response = await callWith(app, "/v1/checkout/sessions", {
    method: "POST",
    headers: { "content-type": "application/json", authorization: `Bearer ${sessionToken}` },
    body: JSON.stringify({ request_id: uuidv7() }),
  });
  if (response.status !== 201) throw new Error(`checkout failed: ${response.status}`);
  const sessionId = (await response.json<{ url: string }>()).url.split("/").at(-1);
  if (!sessionId) throw new Error("no session id");
  return { sessionToken, memberId, sessionId };
}

async function purchased(app: App, sessionToken: string): Promise<number> {
  const me = await (await callWith(app, "/v1/me", bearer(sessionToken))).json<{ credits: { purchased: number } }>();
  return me.credits.purchased;
}

async function payment(sessionId: string) {
  return env.DB.prepare(`SELECT status, transaction_id, amount_total_cents, amount_tax_cents FROM payments WHERE id = ?`)
    .bind(sessionId)
    .first<{ status: string; transaction_id: string | null; amount_total_cents: number | null; amount_tax_cents: number | null }>();
}

async function purchaseTransactions(memberId: string): Promise<{ id: string; idempotency_key: string }[]> {
  const { results } = await env.DB.prepare(
    `SELECT id, idempotency_key FROM ledger_transactions WHERE member_id = ? AND kind = 'purchase'`,
  )
    .bind(memberId)
    .all<{ id: string; idempotency_key: string }>();
  return results;
}

describe("POST /v1/stripe/webhook: Checkout events", () => {
  it("grants the credit pack on checkout.session.completed once the payment is paid", async () => {
    const { app } = setup();
    const { sessionToken, memberId, sessionId } = await buyer(app);
    const issuedBefore = await platformBalance("credits_issued");

    const response = await deliver(app, checkoutEvent("completed", { sessionId, memberId }));

    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ received: true });
    expect(await purchased(app, sessionToken)).toBe(1_000_000);
    expect(await platformBalance("credits_issued")).toBe(issuedBefore - 1_000_000);
    const [transaction] = await purchaseTransactions(memberId);
    expect(transaction?.idempotency_key).toBe(`purchase:${sessionId}`);
    // The tax Stripe collected is kept apart from the total, so the VAT owed can be booked.
    expect(await payment(sessionId)).toEqual({
      status: "paid",
      transaction_id: transaction?.id,
      amount_total_cents: 4760,
      amount_tax_cents: 760,
    });
    expect(await unbalancedTransactions()).toEqual([]);
    expect(await driftedBalances()).toEqual([]);
  });

  it("waits for checkout.session.async_payment_succeeded when the payment is still unpaid", async () => {
    const { app } = setup();
    const { sessionToken, memberId, sessionId } = await buyer(app);

    await deliver(app, checkoutEvent("completed", { sessionId, memberId, paymentStatus: "unpaid" }));
    const whileProcessing = await purchased(app, sessionToken);
    const statusWhileProcessing = (await payment(sessionId))?.status;
    await deliver(app, checkoutEvent("async_payment_succeeded", { sessionId, memberId }));

    expect(whileProcessing).toBe(0);
    expect(statusWhileProcessing).toBe("processing");
    expect(await purchased(app, sessionToken)).toBe(1_000_000);
    expect((await payment(sessionId))?.status).toBe("paid");
  });

  it("records checkout.session.async_payment_failed and grants nothing", async () => {
    const { app } = setup();
    const { sessionToken, memberId, sessionId } = await buyer(app);

    await deliver(app, checkoutEvent("completed", { sessionId, memberId, paymentStatus: "unpaid" }));
    const response = await deliver(app, checkoutEvent("async_payment_failed", { sessionId, memberId }));

    expect(response.status).toBe(200);
    expect(await purchased(app, sessionToken)).toBe(0);
    expect((await payment(sessionId))?.status).toBe("failed");
  });

  it("leaves a completed session with nothing to pay alone: it grants nothing and waits for no async event", async () => {
    const { app } = setup();
    const { sessionToken, memberId, sessionId } = await buyer(app);

    const response = await deliver(app, checkoutEvent("completed", { sessionId, memberId, paymentStatus: "no_payment_required" }));

    expect(response.status).toBe(200);
    expect(await purchased(app, sessionToken)).toBe(0);
    expect((await payment(sessionId))?.status).toBe("open");
  });

  it("records checkout.session.expired and grants nothing", async () => {
    const { app } = setup();
    const { sessionToken, memberId, sessionId } = await buyer(app);

    await deliver(app, checkoutEvent("expired", { sessionId, memberId }));

    expect(await purchased(app, sessionToken)).toBe(0);
    expect((await payment(sessionId))?.status).toBe("expired");
  });

  it("never grants one session twice, whichever events arrive and however often", async () => {
    const { app } = setup();
    const { sessionToken, memberId, sessionId } = await buyer(app);
    const completed = checkoutEvent("completed", { sessionId, memberId });

    await deliver(app, completed);
    await deliver(app, completed);
    await deliver(app, checkoutEvent("completed", { sessionId, memberId }));
    await deliver(app, checkoutEvent("async_payment_succeeded", { sessionId, memberId }));

    expect(await purchased(app, sessionToken)).toBe(1_000_000);
    expect(await purchaseTransactions(memberId)).toHaveLength(1);
  });

  it("keeps a paid session paid when a failure or expiry event arrives late", async () => {
    const { app } = setup();
    const { sessionToken, memberId, sessionId } = await buyer(app);

    await deliver(app, checkoutEvent("completed", { sessionId, memberId }));
    await deliver(app, checkoutEvent("async_payment_failed", { sessionId, memberId }));
    await deliver(app, checkoutEvent("expired", { sessionId, memberId }));

    expect((await payment(sessionId))?.status).toBe("paid");
    expect(await purchased(app, sessionToken)).toBe(1_000_000);
  });

  it.each(["completed", "async_payment_succeeded"] as CheckoutEventType[])(
    "ignores %s for a session Homeport did not create",
    async (type) => {
      const { app } = setup();
      const { sessionToken, memberId } = await buyer(app);

      const response = await deliver(app, checkoutEvent(type, { sessionId: "cs_test_someone_elses", memberId }));

      expect(response.status).toBe(200);
      expect(await purchased(app, sessionToken)).toBe(0);
    },
  );

  it("ignores an event that names another member than the one who opened the session", async () => {
    const { app } = setup();
    const { sessionToken, sessionId } = await buyer(app);
    const other = await verifiedMember();

    await deliver(app, checkoutEvent("completed", { sessionId, memberId: other.memberId }));

    expect(await purchased(app, sessionToken)).toBe(0);
    expect(await purchased(app, other.sessionToken)).toBe(0);
    expect((await payment(sessionId))?.status).toBe("open");
  });

  it("grants what the session was opened for, whatever pack the event names", async () => {
    const { app } = setup();
    const { sessionToken, memberId, sessionId } = await buyer(app);

    await deliver(app, checkoutEvent("completed", { sessionId, memberId, pack: "credits_9000000" }));

    expect(await purchased(app, sessionToken)).toBe(1_000_000);
  });

  it.each([1, 2, 3])(
    "answers 500 when database batch %i fails, and Stripe's redelivery grants the pack exactly once",
    async (failingBatch) => {
      const { app } = setup();
      const { sessionToken, memberId, sessionId } = await buyer(app);
      const event = checkoutEvent("completed", { sessionId, memberId });

      const failed = await deliver(app, event, { DB: dbFailingBatch(failingBatch) });
      // The grant and the payment's status commit together, or neither does.
      const grantedAfterFailure = (await purchased(app, sessionToken)) === 1_000_000;
      const paidAfterFailure = (await payment(sessionId))?.status === "paid";
      const redelivered = await deliver(app, event);

      expect(failed.status).toBe(500);
      expect(paidAfterFailure).toBe(grantedAfterFailure);
      expect(redelivered.status).toBe(200);
      expect(await purchased(app, sessionToken)).toBe(1_000_000);
      expect(await purchaseTransactions(memberId)).toHaveLength(1);
      expect((await payment(sessionId))?.status).toBe("paid");
    },
  );
});
