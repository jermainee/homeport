import { describe, expect, it } from "vitest";
import { expectContract } from "./contract";
import { randomKey, send, verifiedMember } from "./share-fixtures";
import { signIn, uniqueEmail, uuidv7 } from "./helpers";

function register(sessionToken: string, body: Record<string, unknown>) {
  return send("POST", "/v1/devices", sessionToken, { request_id: uuidv7(), name: "Living room Mac mini", platform: "macos", ...body });
}

describe("POST /v1/devices", () => {
  it("stores the agent key and returns the SHA-256 of its SubjectPublicKeyInfo", async () => {
    const { sessionToken } = await verifiedMember();

    // The contract's example key and hash.
    const response = await register(sessionToken, { agent_public_key: "mxYU2PGVkK+Uz1YU2GQMG4JAYyyG8FUXG3yI+jIUxEQ=" });

    expect(response.status).toBe(201);
    const device = await expectContract<{ device_id: string; spki_sha256: string }>("POST", "/v1/devices", response);
    expect(device.spki_sha256).toBe("9cbcb6d9cf5e797e8b18a940a0fd2a645edc317d1dc5f272a988223ae69cc2f8");
  });

  it("returns the same device with 200 when the member registers its key again", async () => {
    const { sessionToken } = await verifiedMember();
    const key = randomKey();
    const first = await (await register(sessionToken, { agent_public_key: key })).json();

    const again = await register(sessionToken, { agent_public_key: key, name: "Renamed" });

    expect(again.status).toBe(200);
    expect(await expectContract("POST", "/v1/devices", again)).toEqual(first);
  });

  it("returns 409 agent_key_taken when another member registered the key", async () => {
    const key = randomKey();
    await register((await verifiedMember()).sessionToken, { agent_public_key: key });

    const response = await register((await verifiedMember()).sessionToken, { agent_public_key: key });

    expect(response.status).toBe(409);
    expect(await expectContract("POST", "/v1/devices", response)).toEqual({ error: "agent_key_taken" });
  });

  it("lets a member register a device before the identity check, as the contract allows", async () => {
    const { sessionToken } = await signIn(uniqueEmail());

    const response = await register(sessionToken, { agent_public_key: randomKey() });

    expect(response.status).toBe(201);
  });

  it.each([
    ["an unknown field", { agent_public_key: "KEY", extra: 1 }],
    ["a key that is not 32 bytes of base64", { agent_public_key: "bm90IGEga2V5" }],
    ["an unknown platform", { agent_public_key: "KEY", platform: "android" }],
    ["an empty name", { agent_public_key: "KEY", name: "" }],
    ["a name over 64 characters", { agent_public_key: "KEY", name: "x".repeat(65) }],
  ])("rejects %s with 400 invalid_request", async (_case, body) => {
    const { sessionToken } = await verifiedMember();
    const withKey = { ...body, agent_public_key: body.agent_public_key === "KEY" ? randomKey() : body.agent_public_key };

    const response = await register(sessionToken, withKey);

    expect(response.status).toBe(400);
    expect(await expectContract("POST", "/v1/devices", response)).toMatchObject({ error: "invalid_request" });
  });

  it("returns 409 idempotency_conflict when another member reuses the request_id", async () => {
    const body = { request_id: uuidv7(), name: "Mac", platform: "macos", agent_public_key: randomKey() };
    await send("POST", "/v1/devices", (await verifiedMember()).sessionToken, body);

    const response = await send("POST", "/v1/devices", (await verifiedMember()).sessionToken, body);

    expect(response.status).toBe(409);
    expect(await expectContract("POST", "/v1/devices", response)).toEqual({
      error: "idempotency_conflict",
      message: expect.any(String),
    });
  });

  it("allows 10 new devices a day, then answers 429 with Retry-After", async () => {
    const { sessionToken } = await verifiedMember();
    for (let i = 0; i < 10; i++) {
      expect((await register(sessionToken, { agent_public_key: randomKey() })).status).toBe(201);
    }

    const response = await register(sessionToken, { agent_public_key: randomKey() });

    expect(response.status).toBe(429);
    expect(Number(response.headers.get("retry-after"))).toBeGreaterThan(0);
    expect(await expectContract("POST", "/v1/devices", response)).toMatchObject({ error: "rate_limited" });
  });

  it("returns 401 without a session", async () => {
    const response = await send("POST", "/v1/devices", "hpm_not-a-session", { request_id: uuidv7() });

    expect(response.status).toBe(401);
  });
});
