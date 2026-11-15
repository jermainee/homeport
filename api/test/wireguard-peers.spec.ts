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
  failover: "country" | "city" | "off";
  https_only: boolean;
  exit_line: { line_id: string; country: string | null; city: string | null; since: string } | null;
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

    expect(peer).toMatchObject({ name: "iPhone", line_id: lineId, failover: "country", https_only: true, exit_line: null, public_key: publicKey });
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

  it("leaves HTTPS only off when the request turns it off, and refuses anything but a boolean", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();
    const body = (httpsOnly: unknown) => ({ request_id: uuidv7(), line_id: lineId, name: "Router", public_key: randomKey(), https_only: httpsOnly });

    const off = await send("POST", "/v1/wireguard/peers", sessionToken, body(false));
    const bad = await send("POST", "/v1/wireguard/peers", sessionToken, body("yes"));

    expect(off.status).toBe(201);
    const { peer } = await expectContract<{ peer: Peer }>("POST", "/v1/wireguard/peers", off);
    expect(peer.https_only).toBe(false);
    expect(bad.status).toBe(400);
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

  it("allows a member 20 active peers, then answers 429, so no one can use up the addresses", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();
    const peers = [];
    for (let i = 0; i < 20; i++) peers.push((await createdPeer(sessionToken, lineId)).peer);

    const over = await createPeer(sessionToken, lineId);
    await send("DELETE", `/v1/wireguard/peers/${peers[0]?.peer_id ?? ""}`, sessionToken);
    const afterDelete = await createPeer(sessionToken, lineId);

    expect(over.status).toBe(429);
    expect(await expectContract("POST", "/v1/wireguard/peers", over)).toMatchObject({ error: "rate_limited" });
    expect(afterDelete.status).toBe(201);
  });

  it("refuses a line whose owner is suspended", async () => {
    const owner = await verifiedMember();
    const lineId = await sharingLine(owner.sessionToken, BERLIN);
    await env.DB.prepare(`UPDATE members SET status = 'suspended' WHERE id = ?`).bind(owner.memberId).run();
    const { sessionToken } = await verifiedMember();

    const response = await createPeer(sessionToken, lineId);
    const listed = await (await send("GET", "/v1/lines/available", sessionToken)).json<{ lines: { line_id: string }[] }>();

    expect(response.status).toBe(404);
    expect(listed.lines.map((line) => line.line_id)).not.toContain(lineId);
  });

  it("counts a name's length in characters, as the contract does", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();

    const emoji = await createPeer(sessionToken, lineId, randomKey(), "📱".repeat(64));
    const tooLong = await createPeer(sessionToken, lineId, randomKey(), "📱".repeat(65));

    expect(emoji.status).toBe(201);
    expect(tooLong.status).toBe(400);
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

describe("PATCH /v1/wireguard/peers/{peer_id}", () => {
  function update(sessionToken: string, peerId: string, body: unknown) {
    return send("PATCH", `/v1/wireguard/peers/${peerId}`, sessionToken, body);
  }

  async function updated(sessionToken: string, peerId: string, body: unknown): Promise<Peer> {
    const response = await update(sessionToken, peerId, body);
    expect(response.status).toBe(200);
    return expectContract<Peer>("PATCH", "/v1/wireguard/peers/{peer_id}", response);
  }

  it("moves a peer to another line: same key and addresses, so the device keeps its config", async () => {
    const first = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const second = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();
    const { peer } = await createdPeer(sessionToken, first);

    const moved = await updated(sessionToken, peer.peer_id, { line_id: second });

    expect(moved).toEqual({ ...peer, line_id: second });
    const { peers } = await (await send("GET", "/v1/wireguard/peers", sessionToken)).json<{ peers: Peer[] }>();
    expect(peers).toEqual([moved]);
  });

  it("changes a peer's failover", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();
    const { peer } = await createdPeer(sessionToken, lineId);

    const city = await updated(sessionToken, peer.peer_id, { failover: "city" });
    const off = await updated(sessionToken, peer.peer_id, { failover: "off", line_id: lineId });

    expect(city).toEqual({ ...peer, failover: "city" });
    expect(off).toEqual({ ...peer, failover: "off" });
  });

  it("turns a peer's HTTPS only off and on, and refuses anything but a boolean", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();
    const { peer } = await createdPeer(sessionToken, lineId);

    const off = await updated(sessionToken, peer.peer_id, { https_only: false });
    const on = await updated(sessionToken, peer.peer_id, { https_only: true, failover: "city" });
    const bad = await update(sessionToken, peer.peer_id, { https_only: 1 });

    expect(off).toEqual({ ...peer, https_only: false });
    expect(on).toEqual({ ...peer, https_only: true, failover: "city" });
    expect(bad.status).toBe(400);
  });

  it("refuses the member's own line and a linked member's line with 403 line_not_allowed", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const user = await verifiedMember();
    const { peer } = await createdPeer(user.sessionToken, lineId);
    const ownLine = await sharingLine(user.sessionToken, BERLIN);
    const relative = await verifiedMember();
    const relativesLine = await sharingLine(relative.sessionToken, BERLIN);
    await link(relative.memberId, user.memberId, "household");

    const own = await update(user.sessionToken, peer.peer_id, { line_id: ownLine });
    const linked = await update(user.sessionToken, peer.peer_id, { line_id: relativesLine });

    expect([own.status, linked.status]).toEqual([403, 403]);
    expect(await expectContract("PATCH", "/v1/wireguard/peers/{peer_id}", own)).toEqual({ error: "line_not_allowed" });
    expect(await linked.json()).toEqual({ error: "line_not_allowed" });
  });

  it("refuses a full line with 409 line_full, but moves a member to a line they already use", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const full = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const users = [await verifiedMember(), await verifiedMember(), await verifiedMember()];
    for (const user of users) await createdPeer(user.sessionToken, full);
    const { sessionToken } = await verifiedMember();
    const { peer } = await createdPeer(sessionToken, lineId);
    const { peer: secondDevice } = await createdPeer(users[0]?.sessionToken ?? "", lineId);

    const refused = await update(sessionToken, peer.peer_id, { line_id: full });
    const backToItsUsersLine = await update(users[0]?.sessionToken ?? "", secondDevice.peer_id, { line_id: full });

    expect(refused.status).toBe(409);
    expect(await expectContract("PATCH", "/v1/wireguard/peers/{peer_id}", refused)).toEqual({ error: "line_full" });
    expect(backToItsUsersLine.status).toBe(200);
  });

  it("refuses a line on another gateway with 409 line_on_other_gateway: the device's config names its gateway", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const elsewhere = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    await env.DB.prepare(`UPDATE lines SET gateway_id = ? WHERE id = ?`).bind(await seedGateway(), elsewhere).run();
    const { sessionToken } = await verifiedMember();
    const { peer } = await createdPeer(sessionToken, lineId);

    const response = await update(sessionToken, peer.peer_id, { line_id: elsewhere });

    expect(response.status).toBe(409);
    expect(await expectContract("PATCH", "/v1/wireguard/peers/{peer_id}", response)).toEqual({ error: "line_on_other_gateway" });
  });

  it("returns 404 for another member's peer, a deleted peer, an unknown line and a line that is not sharing", async () => {
    const owner = await verifiedMember();
    const lineId = await sharingLine(owner.sessionToken, BERLIN);
    const paused = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const user = await verifiedMember();
    const { peer } = await createdPeer(user.sessionToken, lineId);
    const { peer: deleted } = await createdPeer(user.sessionToken, lineId);
    await send("DELETE", `/v1/wireguard/peers/${deleted.peer_id}`, user.sessionToken);
    const pausedOwner = await env.DB.prepare(`SELECT member_id FROM lines WHERE id = ?`).bind(paused).first<{ member_id: string }>();
    await env.DB.prepare(`UPDATE lines SET sharing_enabled = 0 WHERE id = ? AND member_id = ?`).bind(paused, pausedOwner?.member_id).run();

    const responses = [
      await update((await verifiedMember()).sessionToken, peer.peer_id, { failover: "off" }),
      await update(user.sessionToken, deleted.peer_id, { failover: "off" }),
      await update(user.sessionToken, peer.peer_id, { line_id: crypto.randomUUID() }),
      await update(user.sessionToken, peer.peer_id, { line_id: paused }),
    ];

    expect(responses.map((response) => response.status)).toEqual([404, 404, 404, 404]);
    expect(await expectContract("PATCH", "/v1/wireguard/peers/{peer_id}", responses[0] as Response)).toEqual({ error: "not_found" });
    const { peers } = await (await send("GET", "/v1/wireguard/peers", user.sessionToken)).json<{ peers: Peer[] }>();
    expect(peers).toEqual([peer]);
  });

  it("returns 404 for a line whose owner is no longer verified, which no gateway serves", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const lapsed = await verifiedMember();
    const lapsedLine = await sharingLine(lapsed.sessionToken, BERLIN);
    await env.DB.prepare(`UPDATE members SET identity_status = 'requires_input' WHERE id = ?`).bind(lapsed.memberId).run();
    const user = await verifiedMember();
    const { peer } = await createdPeer(user.sessionToken, lineId);

    const moved = await update(user.sessionToken, peer.peer_id, { line_id: lapsedLine });
    const created = await createPeer(user.sessionToken, lapsedLine);

    expect([moved.status, created.status]).toEqual([404, 404]);
  });

  it("returns 403 identity_required once the member is no longer verified", async () => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const user = await verifiedMember();
    const { peer } = await createdPeer(user.sessionToken, lineId);
    await env.DB.prepare(`UPDATE members SET identity_status = 'requires_input' WHERE id = ?`).bind(user.memberId).run();

    const response = await update(user.sessionToken, peer.peer_id, { failover: "city" });

    expect(response.status).toBe(403);
    expect(await expectContract("PATCH", "/v1/wireguard/peers/{peer_id}", response)).toEqual({ error: "identity_required" });
  });

  it.each([
    ["an empty body", {}],
    ["an unknown field", { failover: "city", name: "Phone" }],
    ["an unknown failover", { failover: "everywhere" }],
    ["a line_id that is not a string", { line_id: 7 }],
  ])("rejects %s with 400 invalid_request", async (_case, body) => {
    const lineId = await sharingLine((await verifiedMember()).sessionToken, BERLIN);
    const { sessionToken } = await verifiedMember();
    const { peer } = await createdPeer(sessionToken, lineId);

    const response = await update(sessionToken, peer.peer_id, body);

    expect(response.status).toBe(400);
    expect(await expectContract("PATCH", "/v1/wireguard/peers/{peer_id}", response)).toMatchObject({ error: "invalid_request" });
  });
});
