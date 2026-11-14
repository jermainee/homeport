import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { expectContract } from "./contract";
import { BERLIN, MUNICH, PARIS, announcedGateway, gatewayCall, lineOn, peerOn } from "./gateway-fixtures";
import { link, registerDevice, send, sharingLine, verifiedMember } from "./share-fixtures";

interface Config {
  etag: string;
  topup_hostnames: string[];
  failback_seconds: number;
  peers: {
    peer_id: string;
    member_id: string;
    public_key: string;
    ipv4: string;
    ipv6: string;
    line_id: string;
    fallback_line_ids: string[];
  }[];
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
      {
        peer_id: peer.peer_id,
        member_id: user.memberId,
        public_key: peer.public_key,
        ipv4: peer.ipv4,
        ipv6: peer.ipv6,
        line_id: lineId,
        fallback_line_ids: [],
      },
    ]);
  });

  it("tells the gateway how long a peer's own line must be back online before the peer moves back to it", async () => {
    const { token } = await announcedGateway();

    expect((await configBody(token)).failback_seconds).toBe(300);
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

describe("GET /v1/gateway/config, fallback lines", () => {
  async function fallbacksOf(token: string, peerId: string): Promise<string[] | undefined> {
    return (await configBody(token)).peers.find((peer) => peer.peer_id === peerId)?.fallback_line_ids;
  }

  it("lists the lines on this gateway that take over a peer's line: same city first, then same country", async () => {
    const { gatewayId, token } = await announcedGateway();
    const primary = await lineOn(gatewayId, BERLIN);
    const munich = await lineOn(gatewayId, MUNICH);
    const berlin = await lineOn(gatewayId, BERLIN);
    await lineOn(gatewayId, PARIS);
    const elsewhere = await announcedGateway();
    await lineOn(elsewhere.gatewayId, BERLIN);
    const { peer } = await peerOn(primary.lineId);

    expect(await fallbacksOf(token, peer.peer_id)).toEqual([berlin.lineId, munich.lineId]);
  });

  it.each([
    ["the member's own line", "own"],
    ["a line whose owner is linked to the member", "linked"],
    ["a line whose sharing is off", "not sharing"],
    ["a line whose owner is suspended", "suspended owner"],
    ["a line already used by three other members", "full"],
  ])("never offers %s", async (_case, kind) => {
    const { gatewayId, token } = await announcedGateway();
    const primary = await lineOn(gatewayId, BERLIN);
    const { user, peer } = await peerOn(primary.lineId);
    const candidate = await lineOn(gatewayId, BERLIN);
    switch (kind) {
      case "own": {
        const ownLine = await sharingLine(user.sessionToken, BERLIN);
        await env.DB.prepare(`UPDATE lines SET gateway_id = ? WHERE id = ?`).bind(gatewayId, ownLine).run();
        await send("PATCH", `/v1/lines/${candidate.lineId}`, candidate.owner.sessionToken, { sharing_enabled: false });
        break;
      }
      case "linked":
        await link(candidate.owner.memberId, user.memberId, "card");
        break;
      case "not sharing":
        await send("PATCH", `/v1/lines/${candidate.lineId}`, candidate.owner.sessionToken, { sharing_enabled: false });
        break;
      case "suspended owner":
        await env.DB.prepare(`UPDATE members SET status = 'suspended' WHERE id = ?`).bind(candidate.owner.memberId).run();
        break;
      case "full":
        for (let i = 0; i < 3; i++) await peerOn(candidate.lineId);
        break;
    }

    expect(await fallbacksOf(token, peer.peer_id)).toEqual([]);
  });

  it("offers a line the member already uses with another device", async () => {
    const { gatewayId, token } = await announcedGateway();
    const primary = await lineOn(gatewayId, BERLIN);
    const { user, peer } = await peerOn(primary.lineId);
    const other = await lineOn(gatewayId, BERLIN);
    for (let i = 0; i < 2; i++) await peerOn(other.lineId);
    await peerOn(other.lineId, user);

    expect(await fallbacksOf(token, peer.peer_id)).toEqual([other.lineId]);
  });

  it("offers at most five lines, and the same ones in the same order to all of a member's devices", async () => {
    const { gatewayId, token } = await announcedGateway();
    const primary = await lineOn(gatewayId, BERLIN);
    const berlin = [];
    for (let i = 0; i < 3; i++) berlin.push(await lineOn(gatewayId, BERLIN));
    const munich = [];
    for (let i = 0; i < 4; i++) munich.push(await lineOn(gatewayId, MUNICH));
    const { user, peer: phone } = await peerOn(primary.lineId);
    const { peer: laptop } = await peerOn(primary.lineId, user);

    const phoneFallbacks = await fallbacksOf(token, phone.peer_id);

    expect(phoneFallbacks).toHaveLength(5);
    expect(new Set(phoneFallbacks?.slice(0, 3))).toEqual(new Set(berlin.map((line) => line.lineId)));
    expect(munich.map((line) => line.lineId)).toEqual(expect.arrayContaining(phoneFallbacks?.slice(3) ?? []));
    expect(await fallbacksOf(token, laptop.peer_id)).toEqual(phoneFallbacks);
  });

  it("follows the peer's failover: same city only for city, none for off", async () => {
    const { gatewayId, token } = await announcedGateway();
    const primary = await lineOn(gatewayId, BERLIN);
    const berlin = await lineOn(gatewayId, BERLIN);
    await lineOn(gatewayId, MUNICH);
    const { user, peer } = await peerOn(primary.lineId);

    await send("PATCH", `/v1/wireguard/peers/${peer.peer_id}`, user.sessionToken, { failover: "city" });
    const city = await fallbacksOf(token, peer.peer_id);
    await send("PATCH", `/v1/wireguard/peers/${peer.peer_id}`, user.sessionToken, { failover: "off" });
    const off = await fallbacksOf(token, peer.peer_id);

    expect(city).toEqual([berlin.lineId]);
    expect(off).toEqual([]);
  });

  it("serves a moved peer on its new line, with that line's fallbacks", async () => {
    const { gatewayId, token } = await announcedGateway();
    const berlin = await lineOn(gatewayId, BERLIN);
    const munich = await lineOn(gatewayId, MUNICH);
    const otherMunich = await lineOn(gatewayId, MUNICH);
    const { user, peer } = await peerOn(berlin.lineId);

    const moved = await send("PATCH", `/v1/wireguard/peers/${peer.peer_id}`, user.sessionToken, { line_id: munich.lineId });

    expect(moved.status).toBe(200);
    const served = (await configBody(token)).peers.find((listed) => listed.peer_id === peer.peer_id);
    expect(served?.line_id).toBe(munich.lineId);
    expect(served?.fallback_line_ids).toEqual([otherMunich.lineId, berlin.lineId]);
  });
});
