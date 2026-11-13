import { env } from "cloudflare:workers";
import { beforeAll, describe, expect, it } from "vitest";
import { expectContract } from "./contract";
import { signIn, uniqueEmail, uuidv7 } from "./helpers";
import { GATEWAY, link, randomKey, seedGateway, send, sharingLine, verifiedMember } from "./share-fixtures";

beforeAll(async () => {
  await seedGateway();
});

const BERLIN: Partial<IncomingRequestCfProperties> = { country: "DE", city: "Berlin", asOrganization: "Deutsche Telekom AG" };

interface Peer {
  peer_id: string;
  name: string;
  line_id: string;
  public_key: string;
  ipv4: string;
  ipv6: string;
  created_at: string;
}

function createPeer(sessionToken: string, lineId: string, publicKey = randomKey(), name = "iPhone") {
  return send("POST", "/v1/wireguard/peers", sessionToken, { request_id: uuidv7(), line_id: lineId, name, public_key: publicKey });
}

async function createdPeer(sessionToken: string, lineId: string, publicKey?: string) {
  const response = await createPeer(sessionToken, lineId, publicKey);
  expect(response.status).toBe(201);
  return expectContract<{ peer: Peer; config_text: string }>("POST", "/v1/wireguard/peers", response);
}

/** The host number of a tunnel address: 10.77.a.b is a × 256 + b. */
function hostNumber(ipv4: string): number {
  const [first, second, a, b] = ipv4.split(".").map(Number);
  expect([first, second]).toEqual([10, 77]);
  return (a ?? 0) * 256 + (b ?? 0);
}

describe("POST /v1/wireguard/peers", () => {
  it("creates a peer on the line and returns its .conf file, private key left to the client", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();
    const publicKey = randomKey();

    const { peer, config_text } = await createdPeer(sessionToken, lineId, publicKey);

    expect(peer).toMatchObject({ name: "iPhone", line_id: lineId, public_key: publicKey });
    expect(config_text).toBe(
      [
        "[Interface]",
        "PrivateKey = {{PRIVATE_KEY}}",
        `Address = ${peer.ipv4}/32, ${peer.ipv6}/128`,
        "DNS = 10.77.0.1",
        "MTU = 1280",
        "",
        "[Peer]",
        `PublicKey = ${GATEWAY.wg_public_key}`,
        "AllowedIPs = 0.0.0.0/0, ::/0",
        `Endpoint = ${GATEWAY.wg_endpoint}`,
        "PersistentKeepalive = 25",
        "",
      ].join("\n"),
    );
  });

  it("gives each peer its own address in 10.77.0.0/16 and fd77::/64, never the gateway's", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();

    // Concurrent requests, as several devices would send them.
    const peers = await Promise.all(Array.from({ length: 10 }, () => createdPeer(sessionToken, lineId)));

    const hosts = peers.map(({ peer }) => hostNumber(peer.ipv4));
    expect(new Set(hosts).size).toBe(10);
    expect(new Set(peers.map(({ peer }) => peer.ipv6)).size).toBe(10);
    for (const { peer } of peers) {
      const host = hostNumber(peer.ipv4);
      expect(host).toBeGreaterThanOrEqual(2);
      expect(host).toBeLessThanOrEqual(65534);
      // The IPv6 address carries the same host number in fd77::/64.
      expect(peer.ipv6).toBe(`fd77::${host.toString(16)}`);
    }
  });

  it("frees a deleted peer's key for a new peer", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();
    const publicKey = randomKey();
    const { peer } = await createdPeer(sessionToken, lineId, publicKey);
    await send("DELETE", `/v1/wireguard/peers/${peer.peer_id}`, sessionToken);

    const again = await createPeer(sessionToken, lineId, publicKey);

    expect(again.status).toBe(201);
  });

  it("returns 409 public_key_taken when another peer has the key", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const publicKey = randomKey();
    await createdPeer((await verifiedMember()).sessionToken, lineId, publicKey);

    const response = await createPeer((await verifiedMember()).sessionToken, lineId, publicKey);

    expect(response.status).toBe(409);
    expect(await expectContract("POST", "/v1/wireguard/peers", response)).toEqual({ error: "public_key_taken" });
  });

  it("refuses the member's own line with 403 line_not_allowed", async () => {
    const { sessionToken } = await verifiedMember();
    const lineId = await sharingLine(sessionToken, BERLIN);

    const response = await createPeer(sessionToken, lineId);

    expect(response.status).toBe(403);
    expect(await expectContract("POST", "/v1/wireguard/peers", response)).toEqual({ error: "line_not_allowed" });
  });

  it("refuses a line whose owner is linked to the member, in either direction", async () => {
    const owner = await verifiedMember();
    const lineId = await sharingLine(owner.sessionToken, BERLIN);
    const household = await verifiedMember();
    const connect = await verifiedMember();
    await link(household.memberId, owner.memberId, "household");
    await link(owner.memberId, connect.memberId, "connect_account");

    expect((await createPeer(household.sessionToken, lineId)).status).toBe(403);
    expect((await createPeer(connect.sessionToken, lineId)).status).toBe(403);
  });

  it("refuses a line whose owner signed in from the same app install", async () => {
    const owner = await verifiedMember();
    const lineId = await sharingLine(owner.sessionToken, BERLIN);
    const sameMac = await verifiedMember();
    await env.DB.prepare(`UPDATE auth_sessions SET install_id = ? WHERE member_id IN (?, ?)`)
      .bind(crypto.randomUUID(), owner.memberId, sameMac.memberId)
      .run();

    const response = await createPeer(sameMac.sessionToken, lineId);

    expect(response.status).toBe(403);
    expect(await response.json()).toEqual({ error: "line_not_allowed" });
  });

  it("serves at most 3 members on a line, and lets them add more devices", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const users = [await verifiedMember(), await verifiedMember(), await verifiedMember()];
    for (const user of users) await createdPeer(user.sessionToken, lineId);
    const fourth = await verifiedMember();

    const full = await createPeer(fourth.sessionToken, lineId);
    const secondDevice = await createPeer(users[0]?.sessionToken ?? "", lineId);

    expect(full.status).toBe(409);
    expect(await expectContract("POST", "/v1/wireguard/peers", full)).toEqual({ error: "line_full" });
    expect(secondDevice.status).toBe(201);
    const listed = await (await send("GET", "/v1/lines/available", fourth.sessionToken)).json<{ lines: { line_id: string }[] }>();
    expect(listed.lines.map((line) => line.line_id)).not.toContain(lineId);
  });

  it("returns 404 for an unknown line and for a line that is not sharing", async () => {
    const owner = await verifiedMember();
    const lineId = await sharingLine(owner.sessionToken, BERLIN);
    await send("PATCH", `/v1/lines/${lineId}`, owner.sessionToken, { sharing_enabled: false });
    const { sessionToken } = await verifiedMember();

    const paused = await createPeer(sessionToken, lineId);
    const unknown = await createPeer(sessionToken, crypto.randomUUID());

    expect([paused.status, unknown.status]).toEqual([404, 404]);
    expect(await expectContract("POST", "/v1/wireguard/peers", unknown)).toEqual({ error: "not_found" });
  });

  it("returns 403 identity_required before the member is verified", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);

    const response = await createPeer((await signIn(uniqueEmail())).sessionToken, lineId);

    expect(response.status).toBe(403);
    expect(await expectContract("POST", "/v1/wireguard/peers", response)).toEqual({ error: "identity_required" });
  });

  it.each([
    ["an unknown field", { extra: true }],
    ["a malformed key", { public_key: "not-a-key" }],
    ["an empty name", { name: "" }],
  ])("rejects %s with 400 invalid_request", async (_case, override) => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();

    const response = await send("POST", "/v1/wireguard/peers", sessionToken, {
      request_id: uuidv7(),
      line_id: lineId,
      name: "iPhone",
      public_key: randomKey(),
      ...override,
    });

    expect(response.status).toBe(400);
    expect(await expectContract("POST", "/v1/wireguard/peers", response)).toMatchObject({ error: "invalid_request" });
  });
});

describe("GET and DELETE /v1/wireguard/peers", () => {
  it("lists the member's own peers, newest first", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();
    const first = await createdPeer(sessionToken, lineId);
    const second = await createdPeer(sessionToken, lineId);
    await createdPeer((await verifiedMember()).sessionToken, lineId);

    const response = await send("GET", "/v1/wireguard/peers", sessionToken);

    expect(response.status).toBe(200);
    const { peers } = await expectContract<{ peers: Peer[] }>("GET", "/v1/wireguard/peers", response);
    expect(peers).toEqual([second.peer, first.peer]);
  });

  it("deletes a peer: it leaves the list, and deleting it again is 404", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();
    const { peer } = await createdPeer(sessionToken, lineId);

    const deleted = await send("DELETE", `/v1/wireguard/peers/${peer.peer_id}`, sessionToken);
    const again = await send("DELETE", `/v1/wireguard/peers/${peer.peer_id}`, sessionToken);

    expect(deleted.status).toBe(204);
    expect(again.status).toBe(404);
    expect(await expectContract("DELETE", "/v1/wireguard/peers/{peer_id}", again)).toEqual({ error: "not_found" });
    const { peers } = await (await send("GET", "/v1/wireguard/peers", sessionToken)).json<{ peers: Peer[] }>();
    expect(peers).toEqual([]);
  });

  it("returns 404 for another member's peer and leaves it alone", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const owner = await verifiedMember();
    const { peer } = await createdPeer(owner.sessionToken, lineId);

    const response = await send("DELETE", `/v1/wireguard/peers/${peer.peer_id}`, (await verifiedMember()).sessionToken);

    expect(response.status).toBe(404);
    const { peers } = await (await send("GET", "/v1/wireguard/peers", owner.sessionToken)).json<{ peers: Peer[] }>();
    expect(peers.map((listed) => listed.peer_id)).toEqual([peer.peer_id]);
  });

  it("returns 401 without a session", async () => {
    expect((await send("GET", "/v1/wireguard/peers", "hpm_nope")).status).toBe(401);
  });
});
