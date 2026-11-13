import { describe, expect, it } from "vitest";
import { call, signIn, uniqueEmail } from "./helpers";

describe("GET /v1/me", () => {
  it("returns 401 without a session", async () => {
    const response = await call("/v1/me");

    expect(response.status).toBe(401);
    expect(response.headers.get("www-authenticate")).toMatch(/^Bearer/);
    expect(await response.json()).toEqual({ error: "unauthorized" });
  });

  it("returns 401 for an unknown session token", async () => {
    const response = await call("/v1/me", {
      headers: { authorization: "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" },
    });

    expect(response.status).toBe(401);
  });

  it("returns 401 when the token is not sent as a Bearer token", async () => {
    const { sessionToken } = await signIn(uniqueEmail());

    const response = await call("/v1/me", { headers: { authorization: sessionToken } });

    expect(response.status).toBe(401);
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
        const response = await call("/v1/me", { headers: { authorization: `Bearer ${token}` } });
        return response.json<{ id: string; email: string }>();
      }),
    );

    expect(members[1]).toEqual(members[0]);
    expect(members[0]?.email).toBe(email);
  });
});

describe("DELETE /v1/auth/session", () => {
  it("signs out: the session token stops working", async () => {
    const { sessionToken } = await signIn(uniqueEmail());
    const auth = { authorization: `Bearer ${sessionToken}` };

    const signOut = await call("/v1/auth/session", { method: "DELETE", headers: auth });

    expect(signOut.status).toBe(204);
    expect((await call("/v1/me", { headers: auth })).status).toBe(401);
  });

  it("leaves the member's other sessions signed in", async () => {
    const email = uniqueEmail();
    const laptop = await signIn(email);
    const desktop = await signIn(email);

    await call("/v1/auth/session", {
      method: "DELETE",
      headers: { authorization: `Bearer ${laptop.sessionToken}` },
    });

    const me = await call("/v1/me", { headers: { authorization: `Bearer ${desktop.sessionToken}` } });
    expect(me.status).toBe(200);
  });

  it("returns 401 without a session", async () => {
    expect((await call("/v1/auth/session", { method: "DELETE" })).status).toBe(401);
  });
});
