import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { call } from "./helpers";
import { GATEWAY } from "./share-fixtures";

function putGateway(id: string, body: unknown, envOverrides: Partial<Env> = {}) {
  return call(
    `/dev/gateways/${id}`,
    { method: "PUT", headers: { "content-type": "application/json" }, body: JSON.stringify(body) },
    envOverrides,
  );
}

async function stored(id: string) {
  return env.DB.prepare(`SELECT wg_endpoint, version FROM gateways WHERE id = ?`).bind(id).first();
}

describe("PUT /dev/gateways/{id}", () => {
  it("stores a gateway, and replaces it when sent again", async () => {
    const id = crypto.randomUUID();

    const first = await putGateway(id, GATEWAY);
    const second = await putGateway(id, { ...GATEWAY, version: "0.2.0" });

    expect([first.status, second.status]).toEqual([204, 204]);
    expect(await stored(id)).toEqual({ wg_endpoint: GATEWAY.wg_endpoint, version: "0.2.0" });
  });

  it.each([
    ["a missing field", { ...GATEWAY, version: undefined }],
    ["an unknown field", { ...GATEWAY, region: "fra" }],
    ["a malformed key", { ...GATEWAY, wg_public_key: "nope" }],
    ["an endpoint without a port", { ...GATEWAY, wg_endpoint: "fra1.gw.homeport.example" }],
  ])("rejects %s with 400", async (_case, body) => {
    const response = await putGateway(crypto.randomUUID(), body);

    expect(response.status).toBe(400);
  });

  it.each(["production", ""])("returns 404 and stores nothing when ENVIRONMENT=%j", async (environment) => {
    const id = crypto.randomUUID();

    const response = await putGateway(id, GATEWAY, { ENVIRONMENT: environment });

    expect(response.status).toBe(404);
    expect(await stored(id)).toBeNull();
  });
});
