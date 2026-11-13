import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { expectContract } from "./contract";
import { FakeStripeCheckout, FakeStripeIdentity, appWith, checkoutEvent, identityEvent, stripeSignature } from "./fake-stripe";
import { GATEWAY_SELF, type LeaseBody, provisionGateway, usageEvent } from "./gateway-fixtures";
import { bearer, callWith, signIn, uniqueEmail, uuidv7 } from "./helpers";
import { driftedBalances, platformBalance, unbalancedTransactions } from "./ledger-fixtures";
import { randomKey } from "./share-fixtures";

/**
 * The plan's control-plane check, through the Worker's own endpoints: two
 * members sign in and pass Stripe Identity (signed webhooks), one buys a
 * credit pack (signed Checkout webhook) and uses the other's line through a
 * gateway, which leases and reports 123,456,789 bytes in five events, one of
 * them sent twice.
 */
describe("billing end to end", () => {
  const stripe = new FakeStripeIdentity();
  const checkout = new FakeStripeCheckout();
  const app = appWith(stripe, checkout);

  function member(path: string, sessionToken: string, method = "GET", body?: unknown) {
    return callWith(app, path, {
      method,
      headers: { authorization: `Bearer ${sessionToken}`, ...(body === undefined ? {} : { "content-type": "application/json" }) },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
  }

  function gateway(token: string, method: string, path: string, body?: unknown) {
    return callWith(app, path, {
      method,
      headers: { authorization: `Bearer ${token}`, ...(body === undefined ? {} : { "content-type": "application/json" }) },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
  }

  async function webhook(event: unknown) {
    const payload = JSON.stringify(event);
    const response = await callWith(app, "/v1/stripe/webhook", {
      method: "POST",
      headers: { "content-type": "application/json", "stripe-signature": await stripeSignature(payload) },
      body: payload,
    });
    expect(response.status).toBe(200);
  }

  async function verifiedThroughStripe() {
    const signedIn = await signIn(uniqueEmail());
    expect((await member("/v1/identity/sessions", signedIn.sessionToken, "POST", { request_id: uuidv7() })).status).toBe(201);
    const sessionId = (await env.DB.prepare(`SELECT id FROM identity_sessions WHERE member_id = ?`).bind(signedIn.memberId).first<{ id: string }>())?.id;
    if (!sessionId) throw new Error("no identity session");
    await webhook(identityEvent("verified", { sessionId, memberId: signedIn.memberId }));
    const me = await (await member("/v1/me", signedIn.sessionToken)).json<{ identity_status: string }>();
    expect(me.identity_status).toBe("verified");
    return signedIn;
  }

  async function credits(sessionToken: string) {
    const me = await member("/v1/me", sessionToken);
    return (await expectContract<{ credits: Record<string, number> }>("GET", "/v1/me", me)).credits;
  }

  it("debits the user 1,234 credits, pays the owner 617 and keeps 617, and every transaction balances", async () => {
    // 1. Both members sign in and pass Stripe Identity.
    const owner = await verifiedThroughStripe();
    const user = await verifiedThroughStripe();

    // 2. The user buys a credit pack; Stripe confirms it by webhook.
    const bought = await member("/v1/checkout/sessions", user.sessionToken, "POST", { request_id: uuidv7() });
    const checkoutSessionId = (await bought.json<{ url: string }>()).url.split("/").at(-1) ?? "";
    await webhook(checkoutEvent("completed", { sessionId: checkoutSessionId, memberId: user.memberId }));
    expect(await credits(user.sessionToken)).toEqual({ purchased: 1_000_000, earned_pending: 0, earned_cashable: 0 });

    // 3. A gateway announces itself; the owner shares a line on it.
    const { gatewayId, token } = await provisionGateway();
    expect((await gateway(token, "PUT", "/v1/gateway/self", GATEWAY_SELF)).status).toBe(204);
    const device = await (
      await member("/v1/devices", owner.sessionToken, "POST", { request_id: uuidv7(), name: "Mac mini", platform: "macos", agent_public_key: randomKey() })
    ).json<{ device_id: string }>();
    const line = await (await member("/v1/lines", owner.sessionToken, "POST", { request_id: uuidv7(), device_id: device.device_id })).json<{ line_id: string }>();
    // New lines go to the newest gateway; pin this one in case another test announced one since.
    await env.DB.prepare(`UPDATE lines SET gateway_id = ? WHERE id = ?`).bind(gatewayId, line.line_id).run();
    expect((await member(`/v1/lines/${line.line_id}`, owner.sessionToken, "PATCH", { sharing_enabled: true })).status).toBe(200);

    // 4. The user adds a WireGuard peer on it, and the gateway's config lists it.
    const created = await member("/v1/wireguard/peers", user.sessionToken, "POST", {
      request_id: uuidv7(),
      line_id: line.line_id,
      name: "iPhone",
      public_key: randomKey(),
    });
    const { peer } = await created.json<{ peer: { peer_id: string } }>();
    const config = await expectContract<{ peers: { peer_id: string }[] }>("GET", "/v1/gateway/config", await gateway(token, "GET", "/v1/gateway/config"));
    expect(config.peers.map((listed) => listed.peer_id)).toEqual([peer.peer_id]);

    // 5. The gateway takes a lease for the peer's session.
    const sessionId = uuidv7();
    const leased = await gateway(token, "POST", "/v1/gateway/leases", { request_id: uuidv7(), peer_id: peer.peer_id, session_id: sessionId });
    expect(leased.status).toBe(201);
    const lease = await expectContract<LeaseBody>("POST", "/v1/gateway/leases", leased);

    // 6. It reports 123,456,789 bytes in five events; the third is sent twice.
    const platformBefore = await platformBalance("platform_revenue");
    const bytes: [number, number][] = [
      [4_000_000, 36_000_000],
      [10_000_000, 40_000_000],
      [3_000_000, 30_000_000],
      [56_000, 400_000],
      [89, 700],
    ];
    const events = bytes.map(([up, down], seq) =>
      usageEvent({ sessionId, peerId: peer.peer_id, lineId: line.line_id, leaseId: lease.lease_id, seq, bytesUp: up, bytesDown: down, final: seq === 4 }),
    );
    const first = await gateway(token, "POST", "/v1/gateway/usage", { events: events.slice(0, 3) });
    const second = await gateway(token, "POST", "/v1/gateway/usage", { events: [events[2], ...events.slice(3)] });
    const statuses = [
      ...(await expectContract<{ results: { status: string }[] }>("POST", "/v1/gateway/usage", first)).results,
      ...(await expectContract<{ results: { status: string }[] }>("POST", "/v1/gateway/usage", second)).results,
    ].map((result) => result.status);
    expect(statuses).toEqual(["applied", "applied", "applied", "duplicate", "applied", "applied"]);

    // 7. The session ends: the gateway returns the lease.
    const returned = await gateway(token, "POST", `/v1/gateway/leases/${lease.lease_id}/return`, { request_id: uuidv7() });
    expect(await expectContract("POST", "/v1/gateway/leases/{lease_id}/return", returned)).toEqual({ released_credits: 2_000 - 1_234 });

    expect(await credits(user.sessionToken)).toEqual({ purchased: 1_000_000 - 1_234, earned_pending: 0, earned_cashable: 0 });
    expect(await credits(owner.sessionToken)).toEqual({ purchased: 0, earned_pending: 617, earned_cashable: 0 });
    expect((await platformBalance("platform_revenue")) - platformBefore).toBe(617);
    expect(await unbalancedTransactions()).toEqual([]);
    expect(await driftedBalances()).toEqual([]);

    const logged = await env.DB.prepare(`SELECT member_id, line_id, bytes_up, bytes_down, ended FROM usage_sessions WHERE session_id = ?`)
      .bind(sessionId)
      .first();
    expect(logged).toEqual({ member_id: user.memberId, line_id: line.line_id, bytes_up: 17_056_089, bytes_down: 106_400_700, ended: 1 });
  });
});
