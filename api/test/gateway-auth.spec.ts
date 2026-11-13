import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { expectContract } from "./contract";
import { GATEWAY_SELF, gatewayCall, provisionGateway } from "./gateway-fixtures";
import { bearer, call, signIn, uniqueEmail } from "./helpers";

describe("gateway token", () => {
  it("is stored only as the SHA-256 of its secret", async () => {
    const { gatewayId, token } = await provisionGateway();
    const secret = token.split(".")[1] ?? "";

    const row = await env.DB.prepare(`SELECT * FROM gateway_credentials WHERE gateway_id = ?`)
      .bind(gatewayId)
      .first();

    expect(token).toMatch(/^hpgw_[0-9a-f-]{36}\.[A-Za-z0-9_-]{43}$/);
    expect(JSON.stringify(row)).not.toContain(secret);
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(secret));
    expect(row?.["secret_sha256"]).toBe(Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join(""));
  });

  it("opens the gateway endpoints", async () => {
    const { token } = await provisionGateway();

    const response = await gatewayCall(token, "GET", "/v1/gateway/config");

    expect(response.status).toBe(200);
  });

  it.each([
    ["no header", null],
    ["a wrong secret", "wrong"],
    ["another gateway's id", "other-id"],
    ["a malformed token", "malformed"],
    ["a revoked token", "revoked"],
  ])("answers 401 for %s", async (_case, variant) => {
    const { gatewayId, token } = await provisionGateway();
    const other = await provisionGateway();
    let header: string | null = token;
    if (variant === null) header = null;
    if (variant === "wrong") header = `${token.slice(0, -4)}AAAA`;
    if (variant === "other-id") header = `hpgw_${other.gatewayId}.${token.split(".")[1] ?? ""}`;
    if (variant === "malformed") header = `hpgw_${gatewayId}`;
    if (variant === "revoked") {
      await env.DB.prepare(`UPDATE gateway_credentials SET revoked_at = ? WHERE gateway_id = ?`).bind(Date.now(), gatewayId).run();
    }

    const response = await call("/v1/gateway/config", header === null ? {} : bearer(header));

    expect(response.status).toBe(401);
    expect(response.headers.get("www-authenticate")).toMatch(/^Bearer/);
    expect(await expectContract("GET", "/v1/gateway/config", response)).toEqual({ error: "unauthorized" });
  });

  it("answers 401 to a member's session token", async () => {
    const { sessionToken } = await signIn(uniqueEmail());

    const response = await call("/v1/gateway/config", bearer(sessionToken));

    expect(response.status).toBe(401);
  });

  it("does not open member endpoints", async () => {
    const { token } = await provisionGateway();

    expect((await call("/v1/me", bearer(token))).status).toBe(401);
  });
});

describe("PUT /v1/gateway/self", () => {
  it("stores the gateway's endpoints and keys under the token's gateway id", async () => {
    const { gatewayId, token } = await provisionGateway();

    const response = await gatewayCall(token, "PUT", "/v1/gateway/self", GATEWAY_SELF);

    expect(response.status).toBe(204);
    const row = await env.DB.prepare(
      `SELECT wg_public_key, wg_endpoint, quic_endpoint, quic_server_name, quic_spki_sha256, version FROM gateways WHERE id = ?`,
    )
      .bind(gatewayId)
      .first();
    expect(row).toEqual(GATEWAY_SELF);
  });

  it("replaces the previous announcement", async () => {
    const { gatewayId, token } = await provisionGateway();
    await gatewayCall(token, "PUT", "/v1/gateway/self", GATEWAY_SELF);

    await gatewayCall(token, "PUT", "/v1/gateway/self", { ...GATEWAY_SELF, wg_endpoint: "fra2.gw.homeport.example:51820", version: "0.2.0" });

    const row = await env.DB.prepare(`SELECT wg_endpoint, version FROM gateways WHERE id = ?`).bind(gatewayId).first();
    expect(row).toEqual({ wg_endpoint: "fra2.gw.homeport.example:51820", version: "0.2.0" });
  });

  it.each([
    ["an unknown field", { ...GATEWAY_SELF, region: "fra" }],
    ["a missing field", { ...GATEWAY_SELF, version: undefined }],
    ["a malformed key", { ...GATEWAY_SELF, wg_public_key: "not-a-key" }],
    ["an uppercase pin", { ...GATEWAY_SELF, quic_spki_sha256: GATEWAY_SELF.quic_spki_sha256.toUpperCase() }],
  ])("rejects %s with 400", async (_case, body) => {
    const { token } = await provisionGateway();

    const response = await gatewayCall(token, "PUT", "/v1/gateway/self", body);

    expect(response.status).toBe(400);
    expect(await expectContract("PUT", "/v1/gateway/self", response)).toMatchObject({ error: "invalid_request" });
  });
});
