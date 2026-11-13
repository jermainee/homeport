import { describe, expect, it } from "vitest";
import { expectContract } from "./contract";
import { FakeStripeIdentity, appWith } from "./fake-stripe";
import { callWith, signIn, uniqueEmail, uuidv7 } from "./helpers";
import { env } from "cloudflare:workers";

function startCheck(app: ReturnType<typeof appWith>, sessionToken: string | null, body: unknown) {
  return callWith(app, "/v1/identity/sessions", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      ...(sessionToken ? { authorization: `Bearer ${sessionToken}` } : {}),
    },
    body: JSON.stringify(body),
  });
}

describe("POST /v1/identity/sessions", () => {
  it("creates a Stripe Identity session for the member and returns its URL", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const { sessionToken, memberId } = await signIn(uniqueEmail());
    const requestId = uuidv7();

    const response = await startCheck(app, sessionToken, { request_id: requestId });

    expect(response.status).toBe(201);
    const body = await expectContract<{ url: string }>("POST", "/v1/identity/sessions", response);
    expect(body.url).toMatch(/^https:\/\/verify\.stripe\.com\//);
    expect(stripe.created).toEqual([{ memberId, idempotencyKey: `identity_session:${requestId}` }]);
  });

  it("returns the same session to a retry without asking Stripe again", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const { sessionToken } = await signIn(uniqueEmail());
    const body = { request_id: uuidv7() };

    const first = await (await startCheck(app, sessionToken, body)).json();
    const retry = await startCheck(app, sessionToken, body);

    expect(retry.status).toBe(201);
    expect(await retry.json()).toEqual(first);
    expect(stripe.created).toHaveLength(1);
  });

  it("never shows one member's session to another member who reuses the request_id", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const first = await signIn(uniqueEmail());
    const second = await signIn(uniqueEmail());
    const body = { request_id: uuidv7() };
    await startCheck(app, first.sessionToken, body);

    const response = await startCheck(app, second.sessionToken, body);

    expect(response.status).toBe(409);
    expect(await expectContract("POST", "/v1/identity/sessions", response)).toMatchObject({
      error: "idempotency_conflict",
    });
  });

  it("returns 409 identity_already_verified once the member is verified", async () => {
    const stripe = new FakeStripeIdentity();
    const { sessionToken, memberId } = await signIn(uniqueEmail());
    await env.DB.prepare(`UPDATE members SET identity_status = 'verified' WHERE id = ?`).bind(memberId).run();

    const response = await startCheck(appWith(stripe), sessionToken, { request_id: uuidv7() });

    expect(response.status).toBe(409);
    expect(await expectContract("POST", "/v1/identity/sessions", response)).toEqual({
      error: "identity_already_verified",
    });
    expect(stripe.created).toEqual([]);
  });

  it("allows 5 checks a day, then answers 429 with Retry-After", async () => {
    const stripe = new FakeStripeIdentity();
    const app = appWith(stripe);
    const { sessionToken } = await signIn(uniqueEmail());
    for (let check = 0; check < 5; check++) {
      expect((await startCheck(app, sessionToken, { request_id: uuidv7() })).status).toBe(201);
    }

    const response = await startCheck(app, sessionToken, { request_id: uuidv7() });

    expect(response.status).toBe(429);
    expect(Number(response.headers.get("retry-after"))).toBeGreaterThan(0);
    expect(await expectContract("POST", "/v1/identity/sessions", response)).toMatchObject({ error: "rate_limited" });
    expect(stripe.created).toHaveLength(5);
  });

  it("returns 401 without a session", async () => {
    const response = await startCheck(appWith(new FakeStripeIdentity()), null, { request_id: uuidv7() });

    expect(response.status).toBe(401);
  });

  it("rejects an unknown field with 400 invalid_request", async () => {
    const { sessionToken } = await signIn(uniqueEmail());

    const response = await startCheck(appWith(new FakeStripeIdentity()), sessionToken, {
      request_id: uuidv7(),
      return_url: "https://example.com",
    });

    expect(response.status).toBe(400);
    expect(await expectContract("POST", "/v1/identity/sessions", response)).toMatchObject({ error: "invalid_request" });
  });
});
