import { env } from "cloudflare:workers";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  CHECKOUT_SESSION_WINDOW_MS,
  CREDIT_PACK_CREDITS,
  CREDIT_PACK_CURRENCY,
  CREDIT_PACK_ID,
  CREDIT_PACK_NET_CENTS,
  MAX_CHECKOUT_SESSIONS_PER_WINDOW,
} from "../src/pricing";
import { expectContract } from "./contract";
import { FakeStripeCheckout, FakeStripeIdentity, appWith } from "./fake-stripe";
import { callWith, signIn, uniqueEmail, uuidv7 } from "./helpers";
import { verifiedMember } from "./share-fixtures";

type App = ReturnType<typeof appWith>;

function buy(app: App, sessionToken: string | null, body: unknown): Promise<Response> {
  return callWith(app, "/v1/checkout/sessions", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      ...(sessionToken === null ? {} : { authorization: `Bearer ${sessionToken}` }),
    },
    body: JSON.stringify(body),
  });
}

function setup() {
  const checkout = new FakeStripeCheckout();
  return { checkout, app: appWith(new FakeStripeIdentity(), checkout) };
}

interface PaymentRow {
  id: string;
  member_id: string;
  pack: string;
  credits: number;
  net_cents: number;
  currency: string;
  status: string;
  transaction_id: string | null;
}

async function paymentsOf(memberId: string): Promise<PaymentRow[]> {
  const { results } = await env.DB.prepare(
    `SELECT id, member_id, pack, credits, net_cents, currency, status, transaction_id
       FROM payments WHERE member_id = ? ORDER BY created_at`,
  )
    .bind(memberId)
    .all<PaymentRow>();
  return results;
}

describe("POST /v1/checkout/sessions", () => {
  it("returns the URL of a Stripe Checkout session for one credit pack", async () => {
    const { app, checkout } = setup();
    const { sessionToken, memberId } = await verifiedMember();
    const requestId = uuidv7();

    const response = await buy(app, sessionToken, { request_id: requestId });

    expect(response.status).toBe(201);
    const body = await expectContract<{ url: string }>("POST", "/v1/checkout/sessions", response);
    expect(body.url).toMatch(/^https:\/\/checkout\.stripe\.com\/c\/pay\/cs_test_/);
    expect(checkout.created).toEqual([
      {
        memberId,
        pack: CREDIT_PACK_ID,
        credits: CREDIT_PACK_CREDITS,
        netCents: CREDIT_PACK_NET_CENTS,
        currency: CREDIT_PACK_CURRENCY,
        taxCode: "txcd_10000000",
        idempotencyKey: `checkout_session:${requestId}`,
        successUrl: "https://api.homeport.test/checkout/done",
        cancelUrl: "https://api.homeport.test/checkout/canceled",
      },
    ]);
  });

  it("records the open payment with what it will grant, before anyone pays", async () => {
    const { app } = setup();
    const { sessionToken, memberId } = await verifiedMember();

    const response = await buy(app, sessionToken, { request_id: uuidv7() });
    const { url } = await response.json<{ url: string }>();

    expect(await paymentsOf(memberId)).toEqual([
      {
        id: url.split("/").at(-1),
        member_id: memberId,
        pack: CREDIT_PACK_ID,
        credits: 1_000_000,
        net_cents: 4_000,
        currency: "eur",
        status: "open",
        transaction_id: null,
      },
    ]);
  });

  it("answers a retry with the first session and creates no second one", async () => {
    const { app, checkout } = setup();
    const { sessionToken, memberId } = await verifiedMember();
    const body = { request_id: uuidv7() };

    const first = await (await buy(app, sessionToken, body)).json();
    const retry = await buy(app, sessionToken, body);

    expect(retry.status).toBe(201);
    expect(await retry.json()).toEqual(first);
    expect(checkout.created).toHaveLength(1);
    expect(await paymentsOf(memberId)).toHaveLength(1);
  });

  it("answers 403 identity_required until the member passed Stripe Identity", async () => {
    const { app, checkout } = setup();
    const { sessionToken, memberId } = await signIn(uniqueEmail());

    const response = await buy(app, sessionToken, { request_id: uuidv7() });

    expect(response.status).toBe(403);
    expect(await expectContract("POST", "/v1/checkout/sessions", response)).toEqual({ error: "identity_required" });
    expect(checkout.created).toEqual([]);
    expect(await paymentsOf(memberId)).toEqual([]);
  });

  it("answers 401 without a session", async () => {
    const { app } = setup();

    const response = await buy(app, null, { request_id: uuidv7() });

    expect(response.status).toBe(401);
    await expectContract("POST", "/v1/checkout/sessions", response);
  });

  it("rejects an unknown field", async () => {
    const { app, checkout } = setup();
    const { sessionToken } = await verifiedMember();

    const response = await buy(app, sessionToken, { request_id: uuidv7(), credits: 5_000_000 });

    expect(response.status).toBe(400);
    expect(await expectContract("POST", "/v1/checkout/sessions", response)).toMatchObject({ error: "invalid_request" });
    expect(checkout.created).toEqual([]);
  });

  it("answers 500 when Stripe fails, and a retry with the same request_id runs again", async () => {
    const { app, checkout } = setup();
    const { sessionToken, memberId } = await verifiedMember();
    const body = { request_id: uuidv7() };
    checkout.failNextCreate = true;

    const failed = await buy(app, sessionToken, body);
    const retry = await buy(app, sessionToken, body);

    expect(failed.status).toBe(500);
    expect(retry.status).toBe(201);
    expect(await paymentsOf(memberId)).toHaveLength(1);
  });
});

describe("POST /v1/checkout/sessions: limit", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it(`answers 429 with Retry-After after ${MAX_CHECKOUT_SESSIONS_PER_WINDOW} sessions in the window, without calling Stripe`, async () => {
    const { app, checkout } = setup();
    const { sessionToken, memberId } = await verifiedMember();
    for (let i = 0; i < MAX_CHECKOUT_SESSIONS_PER_WINDOW; i++) {
      expect((await buy(app, sessionToken, { request_id: uuidv7() })).status).toBe(201);
    }

    const limited = await buy(app, sessionToken, { request_id: uuidv7() });

    expect(limited.status).toBe(429);
    expect(Number(limited.headers.get("retry-after"))).toBeGreaterThan(0);
    expect(await expectContract("POST", "/v1/checkout/sessions", limited)).toMatchObject({ error: "rate_limited" });
    expect(checkout.created).toHaveLength(MAX_CHECKOUT_SESSIONS_PER_WINDOW);
    expect(await paymentsOf(memberId)).toHaveLength(MAX_CHECKOUT_SESSIONS_PER_WINDOW);
  });

  it("counts each member on their own and frees up once the window has passed", async () => {
    const { app } = setup();
    const busy = await verifiedMember();
    const other = await verifiedMember();
    for (let i = 0; i < MAX_CHECKOUT_SESSIONS_PER_WINDOW; i++) await buy(app, busy.sessionToken, { request_id: uuidv7() });

    const otherMember = await buy(app, other.sessionToken, { request_id: uuidv7() });
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + CHECKOUT_SESSION_WINDOW_MS + 1000);
    const later = await buy(app, busy.sessionToken, { request_id: uuidv7() });

    expect(otherMember.status).toBe(201);
    expect(later.status).toBe(201);
  });
});

describe("Checkout return pages", () => {
  it.each(["/checkout/done", "/checkout/canceled"])("serves %s as an uncached page that sends the member back to the app", async (path) => {
    const { app } = setup();

    const response = await callWith(app, path);

    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toMatch(/^text\/html/);
    expect(response.headers.get("cache-control")).toBe("no-store");
    expect(await response.text()).toMatch(/Homeport app/);
  });
});
