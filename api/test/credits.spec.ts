import { describe, expect, it } from "vitest";
import { chargeUsage } from "../src/ledger";
import { expectContract } from "./contract";
import { createMember } from "./ledger-fixtures";
import { bearer, call, postJson, signIn, uniqueEmail } from "./helpers";
import { env } from "cloudflare:workers";

function grant(memberId: string, body: unknown, envOverrides: Partial<Env> = {}) {
  return postJson(`/dev/members/${memberId}/credits`, body, envOverrides);
}

describe("credits on GET /v1/me", () => {
  it("shows the member's ledger balances", async () => {
    const { sessionToken, memberId } = await signIn(uniqueEmail());
    await grant(memberId, { credits: 1_000_000 });

    const response = await call("/v1/me", bearer(sessionToken));

    const me = await expectContract<{ credits: unknown }>("GET", "/v1/me", response);
    expect(me.credits).toEqual({ purchased: 1_000_000, earned_pending: 0, earned_cashable: 0 });
  });

  it("shows an overdrawn account as 0, as the contract's credits are never negative", async () => {
    const { sessionToken, memberId } = await signIn(uniqueEmail());
    const owner = await createMember();
    await grant(memberId, { credits: 10 });
    await chargeUsage(env.DB, {
      meterId: crypto.randomUUID(),
      payerMemberId: memberId,
      payeeMemberId: owner,
      bytes: 2_000_000, // 20 credits
      usageKey: crypto.randomUUID(),
    });

    const response = await call("/v1/me", bearer(sessionToken));

    const me = await expectContract<{ credits: unknown }>("GET", "/v1/me", response);
    expect(me.credits).toEqual({ purchased: 0, earned_pending: 0, earned_cashable: 0 });
  });
});

describe("POST /dev/members/{member_id}/credits", () => {
  it("grants purchased credits from the dev_grants account", async () => {
    const { sessionToken, memberId } = await signIn(uniqueEmail());

    const response = await grant(memberId, { credits: 2_500 });

    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ credits: { purchased: 2_500, earned_pending: 0, earned_cashable: 0 } });
    const me = await (await call("/v1/me", bearer(sessionToken))).json<{ credits: { purchased: number } }>();
    expect(me.credits.purchased).toBe(2_500);
  });

  it.each([0, -1, 1.5, "10", null])("rejects credits=%j", async (credits) => {
    const memberId = await createMember();

    const response = await grant(memberId, { credits });

    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({ error: "invalid_request" });
  });

  it("returns 404 for an unknown member", async () => {
    const response = await grant(crypto.randomUUID(), { credits: 10 });

    expect(response.status).toBe(404);
  });

  it.each(["production", "staging", ""])("returns 404 and grants nothing when ENVIRONMENT=%j", async (environment) => {
    const { sessionToken, memberId } = await signIn(uniqueEmail());

    const response = await grant(memberId, { credits: 10 }, { ENVIRONMENT: environment });

    expect(response.status).toBe(404);
    const me = await (await call("/v1/me", bearer(sessionToken))).json<{ credits: { purchased: number } }>();
    expect(me.credits.purchased).toBe(0);
  });
});
