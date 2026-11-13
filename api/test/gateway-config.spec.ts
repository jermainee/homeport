import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { expectContract } from "./contract";
import { announcedGateway, gatewayCall, lineOn, peerOn } from "./gateway-fixtures";
import { link, registerDevice, send, sharingLine, verifiedMember } from "./share-fixtures";

interface Config {
  etag: string;
  topup_hostnames: string[];
  peers: { peer_id: string; member_id: string; public_key: string; ipv4: string; ipv6: string; line_id: string }[];
  lines: { line_id: string; owner_member_id: string; device_spki_sha256: string }[];
}

async function config(token: string, headers: Record<string, string> = {}) {
  return gatewayCall(token, "GET", "/v1/gateway/config", undefined, headers);
}

async function configBody(token: string): Promise<Config> {
  const response = await config(token);
  expect(response.status).toBe(200);
  return expectContract<Config>("GET", "/v1/gateway/config", response);
}

describe("GET /v1/gateway/config", () => {
  it("lists the gateway's shared lines and their active peers", async () => {
    const { gatewayId, token } = await announcedGateway();
    const { owner, lineId } = await lineOn(gatewayId);
    const { user, peer } = await peerOn(lineId);
    const device = await env.DB.prepare(`SELECT d.spki_sha256 FROM lines l JOIN devices d ON d.id = l.device_id WHERE l.id = ?`)
      .bind(lineId)
      .first<{ spki_sha256: string }>();

    const body = await configBody(token);

    expect(body.lines).toEqual([{ line_id: lineId, owner_member_id: owner.memberId, device_spki_sha256: device?.spki_sha256 }]);
    expect(body.lines[0]?.device_spki_sha256).toMatch(/^[0-9a-f]{64}$/);
    expect(body.peers).toEqual([
      { peer_id: peer.peer_id, member_id: user.memberId, public_key: peer.public_key, ipv4: peer.ipv4, ipv6: peer.ipv6, line_id: lineId },
    ]);
  });

  it("names the top-up hosts: the API's own host first", async () => {
    const { token } = await announcedGateway();

    const body = await configBody(token);

    expect(body.topup_hostnames[0]).toBe("api.homeport.test");
    expect(body.topup_hostnames).toContain("checkout.stripe.com");
  });

  it("leaves out other gateways' lines and peers", async () => {
    const mine = await announcedGateway();
    const other = await announcedGateway();
    const { lineId } = await lineOn(other.gatewayId);
    await peerOn(lineId);

    const body = await configBody(mine.token);

    expect(body.lines).toEqual([]);
    expect(body.peers).toEqual([]);
  });

  it("drops a deleted peer", async () => {
    const { gatewayId, token } = await announcedGateway();
    const { lineId } = await lineOn(gatewayId);
    const { user, peer } = await peerOn(lineId);

    await send("DELETE", `/v1/wireguard/peers/${peer.peer_id}`, user.sessionToken);

    expect((await configBody(token)).peers).toEqual([]);
  });

  it("drops a line whose sharing is off, with its peers", async () => {
    const { gatewayId, token } = await announcedGateway();
    const { owner, lineId } = await lineOn(gatewayId);
    await peerOn(lineId);

    await send("PATCH", `/v1/lines/${lineId}`, owner.sessionToken, { sharing_enabled: false });

    const body = await configBody(token);
    expect(body.lines).toEqual([]);
    expect(body.peers).toEqual([]);
  });

  it.each([
    ["the peer's member is suspended", "user", "status = 'suspended'"],
    ["the peer's member is no longer verified", "user", "identity_status = 'requires_input'"],
    ["the line's owner is suspended", "owner", "status = 'suspended'"],
  ])("drops a peer when %s", async (_case, who, change) => {
    const { gatewayId, token } = await announcedGateway();
    const { owner, lineId } = await lineOn(gatewayId);
    const { user } = await peerOn(lineId);

    await env.DB.prepare(`UPDATE members SET ${change} WHERE id = ?`)
      .bind(who === "user" ? user.memberId : owner.memberId)
      .run();

    expect((await configBody(token)).peers).toEqual([]);
  });

  it("drops a peer whose member became linked to the line's owner (no self-dealing)", async () => {
    const { gatewayId, token } = await announcedGateway();
    const { owner, lineId } = await lineOn(gatewayId);
    const { user } = await peerOn(lineId);

    await link(user.memberId, owner.memberId, "household");

    const body = await configBody(token);
    expect(body.peers).toEqual([]);
    expect(body.lines.map((line) => line.line_id)).toEqual([lineId]);
  });

  it("answers 304 to If-None-Match with the body's etag, weak or strong, until something changes", async () => {
    const { gatewayId, token } = await announcedGateway();
    const { lineId } = await lineOn(gatewayId);
    const { etag } = await configBody(token);

    const strong = await config(token, { "if-none-match": etag });
    const weak = await config(token, { "if-none-match": `W/${etag}` });
    await peerOn(lineId);
    const changed = await config(token, { "if-none-match": etag });

    expect(strong.status).toBe(304);
    expect(strong.headers.get("etag")).toBe(etag);
    expect(await strong.text()).toBe("");
    expect(weak.status).toBe(304);
    expect(changed.status).toBe(200);
    const body = await expectContract<Config>("GET", "/v1/gateway/config", changed);
    expect(body.etag).not.toBe(etag);
    expect(changed.headers.get("etag")).toBe(body.etag);
  });

  it("keeps the etag while the config stays the same, even when unrelated data changes", async () => {
    const { gatewayId, token } = await announcedGateway();
    const { owner } = await lineOn(gatewayId);

    const first = await configBody(token);
    await registerDevice(owner.sessionToken);
    const second = await configBody(token);

    expect(second.etag).toBe(first.etag);
  });

  it("is closed to a revoked gateway, and new lines go to another gateway", async () => {
    const { gatewayId, token } = await announcedGateway();
    const other = await announcedGateway();
    await env.DB.prepare(`UPDATE gateways SET updated_at = ? WHERE id = ?`).bind(Date.now() + 60_000, gatewayId).run();

    await env.DB.prepare(`UPDATE gateway_credentials SET revoked_at = ? WHERE gateway_id = ?`).bind(Date.now(), gatewayId).run();
    const owner = await verifiedMember();
    const lineId = await sharingLine(owner.sessionToken);

    expect((await config(token)).status).toBe(401);
    const line = await env.DB.prepare(`SELECT gateway_id FROM lines WHERE id = ?`).bind(lineId).first<{ gateway_id: string }>();
    expect(line?.gateway_id).not.toBe(gatewayId);
    expect(other.gatewayId).toBeTruthy();
  });
});
