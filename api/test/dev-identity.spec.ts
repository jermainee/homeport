import { describe, expect, it } from "vitest";
import { bearer, call, signIn, uniqueEmail } from "./helpers";

function verify(memberId: string, envOverrides: Partial<Env> = {}) {
  return call(`/dev/members/${memberId}/identity/verify`, { method: "POST" }, envOverrides);
}

async function identityStatus(sessionToken: string): Promise<string> {
  const me = await (await call("/v1/me", bearer(sessionToken))).json<{ identity_status: string }>();
  return me.identity_status;
}

describe("POST /dev/members/{member_id}/identity/verify", () => {
  it("marks the member's identity verified, as Stripe Identity's webhook would", async () => {
    const { sessionToken, memberId } = await signIn(uniqueEmail());

    const response = await verify(memberId);

    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ identity_status: "verified" });
    expect(await identityStatus(sessionToken)).toBe("verified");
  });

  it("opens what a verified identity gates, such as the available lines", async () => {
    const { sessionToken, memberId } = await signIn(uniqueEmail());
    expect((await call("/v1/lines/available", bearer(sessionToken))).status).toBe(403);

    await verify(memberId);

    expect((await call("/v1/lines/available", bearer(sessionToken))).status).toBe(200);
  });

  it("answers the same when the member is verified already", async () => {
    const { sessionToken, memberId } = await signIn(uniqueEmail());
    await verify(memberId);

    const response = await verify(memberId);

    expect(response.status).toBe(200);
    expect(await identityStatus(sessionToken)).toBe("verified");
  });

  it("returns 404 for an unknown member", async () => {
    const response = await verify(crypto.randomUUID());

    expect(response.status).toBe(404);
    expect(await response.json()).toEqual({ error: "not_found" });
  });

  it.each(["production", "staging", ""])("returns 404 and verifies no one when ENVIRONMENT=%j", async (environment) => {
    const { sessionToken, memberId } = await signIn(uniqueEmail());

    const response = await verify(memberId, { ENVIRONMENT: environment });

    expect(response.status).toBe(404);
    expect(await identityStatus(sessionToken)).toBe("unverified");
  });
});
