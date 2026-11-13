import { describe, expect, it } from "vitest";
import { expectContract } from "./contract";
import { bearer, call, newSessionToken, signIn, uniqueEmail } from "./helpers";

describe("GET /v1/me", () => {
  it("returns 401 without a session", async () => {
    const response = await call("/v1/me");

    expect(response.status).toBe(401);
    expect(response.headers.get("www-authenticate")).toMatch(/^Bearer/);
    expect(await response.json()).toEqual({ error: "unauthorized" });
  });

  it("returns 401 for a session token no sign-in activated", async () => {
    expect((await call("/v1/me", bearer(newSessionToken()))).status).toBe(401);
  });

  it("returns 401 for a gateway token", async () => {
    const response = await call("/v1/me", bearer("hpgw_01a1c4af-8795-7a28-9249-adbd2ba1c04a.secret"));

    expect(response.status).toBe(401);
  });

  it("returns 401 when the token is not sent as a Bearer token", async () => {
    const { sessionToken } = await signIn(uniqueEmail());

    const response = await call("/v1/me", { headers: { authorization: sessionToken } });

    expect(response.status).toBe(401);
  });

  it("returns the member as the contract describes it", async () => {
    const { sessionToken, memberId } = await signIn(uniqueEmail());

    const response = await call("/v1/me", bearer(sessionToken));

    expect(response.status).toBe(200);
    const me = await expectContract("GET", "/v1/me", response);
    expect(me).toMatchObject({ member_id: memberId, identity_status: "unverified" });
  });

  it("accepts the Bearer scheme in any case", async () => {
    const { sessionToken } = await signIn(uniqueEmail());

    const response = await call("/v1/me", { headers: { authorization: `bearer ${sessionToken}` } });

    expect(response.status).toBe(200);
  });

  it("returns the same member for every sign-in with the same email, in any case", async () => {
    const email = uniqueEmail();
    const first = await signIn(email);
    const second = await signIn(email.toUpperCase());

    const members = await Promise.all(
      [first.sessionToken, second.sessionToken].map(async (token) => {
        const response = await call("/v1/me", bearer(token));
        return response.json<{ member_id: string; email: string }>();
      }),
    );

    expect(second.memberId).toBe(first.memberId);
    expect(members[1]).toEqual(members[0]);
    expect(members[0]?.email).toBe(email);
  });
});

describe("DELETE /v1/auth/session", () => {
  it("signs out: the session token stops working", async () => {
    const { sessionToken } = await signIn(uniqueEmail());

    const signOut = await call("/v1/auth/session", { method: "DELETE", ...bearer(sessionToken) });

    expect(signOut.status).toBe(204);
    expect((await call("/v1/me", bearer(sessionToken))).status).toBe(401);
  });

  it("leaves the member's other sessions signed in", async () => {
    const email = uniqueEmail();
    const laptop = await signIn(email);
    const desktop = await signIn(email);

    await call("/v1/auth/session", { method: "DELETE", ...bearer(laptop.sessionToken) });

    expect((await call("/v1/me", bearer(desktop.sessionToken))).status).toBe(200);
  });

  it("returns 401 without a session", async () => {
    expect((await call("/v1/auth/session", { method: "DELETE" })).status).toBe(401);
  });
});
