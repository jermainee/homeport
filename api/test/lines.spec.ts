import { env } from "cloudflare:workers";
import { beforeAll, describe, expect, it } from "vitest";
import { expectContract } from "./contract";
import { signIn, uniqueEmail, uuidv7 } from "./helpers";
import { GATEWAY, link, registerDevice, seedGateway, send, sharingLine, verifiedMember } from "./share-fixtures";

beforeAll(async () => {
  await seedGateway();
});

function createLine(sessionToken: string, deviceId: string, init: RequestInit = {}) {
  return send("POST", "/v1/lines", sessionToken, { request_id: uuidv7(), device_id: deviceId }, init);
}

interface AvailableLine {
  line_id: string;
  country: string;
  city: string | null;
  isp: string | null;
  ipv6: boolean;
  online: boolean;
}

async function available(sessionToken: string): Promise<AvailableLine[]> {
  const response = await send("GET", "/v1/lines/available", sessionToken);
  expect(response.status).toBe(200);
  return (await expectContract<{ lines: AvailableLine[] }>("GET", "/v1/lines/available", response)).lines;
}

const BERLIN: Partial<IncomingRequestCfProperties> = { country: "DE", city: "Berlin", asOrganization: "Deutsche Telekom AG" };

describe("POST /v1/lines", () => {
  it("creates the device's line on a gateway and returns where its agent dials out to", async () => {
    const { sessionToken } = await verifiedMember();
    const deviceId = await registerDevice(sessionToken);

    const response = await createLine(sessionToken, deviceId);

    expect(response.status).toBe(201);
    const line = await expectContract<{ gateway: unknown }>("POST", "/v1/lines", response);
    expect(line.gateway).toEqual({
      quic_endpoint: GATEWAY.quic_endpoint,
      server_name: GATEWAY.quic_server_name,
      spki_sha256: GATEWAY.quic_spki_sha256,
    });
  });

  it("returns the device's existing line with 200: one line per device", async () => {
    const { sessionToken } = await verifiedMember();
    const deviceId = await registerDevice(sessionToken);
    const first = await (await createLine(sessionToken, deviceId)).json();

    const again = await createLine(sessionToken, deviceId);

    expect(again.status).toBe(200);
    expect(await expectContract("POST", "/v1/lines", again)).toEqual(first);
  });

  it("returns 404 for another member's device", async () => {
    const owner = await verifiedMember();
    const deviceId = await registerDevice(owner.sessionToken);

    const response = await createLine((await verifiedMember()).sessionToken, deviceId);

    expect(response.status).toBe(404);
    expect(await expectContract("POST", "/v1/lines", response)).toEqual({ error: "not_found" });
  });

  it("returns 403 identity_required before the member is verified", async () => {
    const { sessionToken } = await signIn(uniqueEmail());
    const deviceId = await registerDevice(sessionToken);

    const response = await createLine(sessionToken, deviceId);

    expect(response.status).toBe(403);
    expect(await expectContract("POST", "/v1/lines", response)).toEqual({ error: "identity_required" });
  });

  it("starts with sharing off", async () => {
    const owner = await verifiedMember();
    const deviceId = await registerDevice(owner.sessionToken);
    const { line_id } = await (await createLine(owner.sessionToken, deviceId)).json<{ line_id: string }>();

    const lines = await available((await verifiedMember()).sessionToken);

    expect(lines.map((line) => line.line_id)).not.toContain(line_id);
  });
});

describe("PATCH /v1/lines/{line_id}", () => {
  it("turns sharing on and off", async () => {
    const { sessionToken } = await verifiedMember();
    const deviceId = await registerDevice(sessionToken);
    const { line_id } = await (await createLine(sessionToken, deviceId)).json<{ line_id: string }>();

    const on = await send("PATCH", `/v1/lines/${line_id}`, sessionToken, { sharing_enabled: true });
    const off = await send("PATCH", `/v1/lines/${line_id}`, sessionToken, { sharing_enabled: false });

    expect(on.status).toBe(200);
    expect(await expectContract("PATCH", "/v1/lines/{line_id}", on)).toEqual({ line_id, sharing_enabled: true });
    expect(await expectContract("PATCH", "/v1/lines/{line_id}", off)).toEqual({ line_id, sharing_enabled: false });
  });

  it("returns 404 for another member's line", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken);

    const response = await send("PATCH", `/v1/lines/${lineId}`, (await verifiedMember()).sessionToken, { sharing_enabled: false });

    expect(response.status).toBe(404);
    expect(await expectContract("PATCH", "/v1/lines/{line_id}", response)).toEqual({ error: "not_found" });
  });

  it.each([{}, { sharing_enabled: "yes" }, { sharing_enabled: true, extra: 1 }])("rejects %j with 400", async (body) => {
    const { sessionToken } = await verifiedMember();
    const lineId = await sharingLine(sessionToken);

    const response = await send("PATCH", `/v1/lines/${lineId}`, sessionToken, body);

    expect(response.status).toBe(400);
    expect(await expectContract("PATCH", "/v1/lines/{line_id}", response)).toMatchObject({ error: "invalid_request" });
  });

  it("returns 403 identity_required before the member is verified", async () => {
    const { sessionToken } = await signIn(uniqueEmail());

    const response = await send("PATCH", `/v1/lines/${crypto.randomUUID()}`, sessionToken, { sharing_enabled: true });

    expect(response.status).toBe(403);
  });
});

describe("GET /v1/lines/available", () => {
  it("lists other members' sharing lines with where they are", async () => {
    const owner = await verifiedMember();
    const lineId = await sharingLine(owner.sessionToken, BERLIN);

    const lines = await available((await verifiedMember()).sessionToken);

    expect(lines.find((line) => line.line_id === lineId)).toEqual({
      line_id: lineId,
      country: "DE",
      city: "Berlin",
      isp: "Deutsche Telekom AG",
      ipv6: false,
      online: false,
    });
  });

  it("never lists the member's own line", async () => {
    const { sessionToken } = await verifiedMember();
    const lineId = await sharingLine(sessionToken, BERLIN);

    expect((await available(sessionToken)).map((line) => line.line_id)).not.toContain(lineId);
  });

  it("never lists a line whose owner is linked to the member, in either direction", async () => {
    const owner = await verifiedMember();
    const lineId = await sharingLine(owner.sessionToken, BERLIN);
    const household = await verifiedMember();
    const card = await verifiedMember();
    await link(household.memberId, owner.memberId, "household");
    await link(owner.memberId, card.memberId, "card");

    expect((await available(household.sessionToken)).map((line) => line.line_id)).not.toContain(lineId);
    expect((await available(card.sessionToken)).map((line) => line.line_id)).not.toContain(lineId);
  });

  it("never lists a line whose owner signed in from the same app install", async () => {
    const owner = await verifiedMember();
    const lineId = await sharingLine(owner.sessionToken, BERLIN);
    const sameMac = await verifiedMember();
    const install = crypto.randomUUID();
    await env.DB.prepare(`UPDATE auth_sessions SET install_id = ? WHERE member_id IN (?, ?)`)
      .bind(install, owner.memberId, sameMac.memberId)
      .run();

    expect((await available(sameMac.sessionToken)).map((line) => line.line_id)).not.toContain(lineId);
  });

  it("skips lines whose country is not known yet", async () => {
    const owner = await verifiedMember();
    const lineId = await sharingLine(owner.sessionToken);

    expect((await available((await verifiedMember()).sessionToken)).map((line) => line.line_id)).not.toContain(lineId);
  });

  it("returns 403 identity_required before the member is verified", async () => {
    const response = await send("GET", "/v1/lines/available", (await signIn(uniqueEmail())).sessionToken);

    expect(response.status).toBe(403);
    expect(await expectContract("GET", "/v1/lines/available", response)).toEqual({ error: "identity_required" });
  });
});
