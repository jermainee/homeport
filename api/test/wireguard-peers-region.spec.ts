import { env } from "cloudflare:workers";
import { beforeAll, describe, expect, it } from "vitest";
import { MAX_USERS_PER_LINE } from "../src/pricing";
import { expectContract } from "./contract";
import { announcedGateway, peerOn } from "./gateway-fixtures";
import { uuidv7 } from "./helpers";
import { countryPool, type Member, onlineLine, type Place } from "./region-fixtures";
import { GATEWAY, link, randomKey, send, sharingLine, verifiedMember } from "./share-fixtures";

interface Peer {
  peer_id: string;
  line_id: string;
  line: { country: string | null; city: string | null; isp: string | null };
}

let gateway: { gatewayId: string; token: string };

beforeAll(async () => {
  gateway = await announcedGateway();
});

const freshCountry = countryPool([
  "XD", "XE", "XF", "XG", "XH", "XI", "XJ", "XK", "XL", "XM", "XN", "XO", "XP", "XQ", "XR", "XS", "XT", "XU", "XV", "XW", "XY", "XZ", "ZZ", "AA",
]);

function lineAt(place: Place, owner?: Member): Promise<string> {
  return onlineLine(gateway, place, owner);
}

function createIn(sessionToken: string, region: Record<string, unknown>) {
  return send("POST", "/v1/wireguard/peers", sessionToken, {
    request_id: uuidv7(),
    name: "iPhone",
    public_key: randomKey(),
    ...region,
  });
}

async function createdIn(sessionToken: string, region: Record<string, unknown>) {
  const response = await createIn(sessionToken, region);
  expect(response.status).toBe(201);
  return expectContract<{ peer: Peer; config_text: string }>("POST", "/v1/wireguard/peers", response);
}

describe("POST /v1/wireguard/peers with a country", () => {
  it("picks a free line in the country, and says where it is", async () => {
    const here = freshCountry();
    const lineId = await lineAt({ country: here, city: "Alpha", isp: "Deutsche Telekom AG" });

    const { peer, config_text } = await createdIn((await verifiedMember()).sessionToken, { country: here });

    expect(peer.line_id).toBe(lineId);
    expect(peer.line).toEqual({ country: here, city: "Alpha", isp: "Deutsche Telekom AG" });
    expect(config_text).toContain(`Endpoint = ${GATEWAY.wg_endpoint}`);
  });

  it("takes https_only with a region, as with a line", async () => {
    const here = freshCountry();
    await lineAt({ country: here, city: "Alpha" });

    const { peer } = await createdIn((await verifiedMember()).sessionToken, { country: here, https_only: false });

    expect((peer as Peer & { https_only: boolean }).https_only).toBe(false);
  });

  it("picks a line in the city when one is named", async () => {
    const here = freshCountry();
    await lineAt({ country: here, city: "Alpha" });
    const beta = await lineAt({ country: here, city: "Beta" });

    const { peer } = await createdIn((await verifiedMember()).sessionToken, { country: here, city: "Beta" });

    expect(peer.line_id).toBe(beta);
  });

  it("picks the line with the fewest other members on it", async () => {
    const here = freshCountry();
    const busy = await lineAt({ country: here, city: "Alpha" });
    await peerOn(busy);
    const quiet = await lineAt({ country: here, city: "Alpha" });

    const { peer } = await createdIn((await verifiedMember()).sessionToken, { country: here, city: "Alpha" });

    expect(peer.line_id).toBe(quiet);
  });

  it("never picks an offline, full, own, linked or unserved line, and answers 409 no_free_line", async () => {
    const here = freshCountry();
    const member = await verifiedMember();
    await sharingLine((await verifiedMember()).sessionToken, { country: here, city: "Alpha" } as Partial<IncomingRequestCfProperties>);
    await lineAt({ country: here, city: "Alpha" }, member);
    const household = await verifiedMember();
    await link(member.memberId, household.memberId, "household");
    await lineAt({ country: here, city: "Alpha" }, household);
    const lapsed = await verifiedMember();
    await lineAt({ country: here, city: "Alpha" }, lapsed);
    await env.DB.prepare(`UPDATE members SET identity_status = 'unverified' WHERE id = ?`).bind(lapsed.memberId).run();
    const full = await lineAt({ country: here, city: "Alpha" });
    for (let i = 0; i < MAX_USERS_PER_LINE; i++) await peerOn(full);

    const response = await createIn(member.sessionToken, { country: here });

    expect(response.status).toBe(409);
    expect(await expectContract("POST", "/v1/wireguard/peers", response)).toMatchObject({ error: "no_free_line" });
  });

  it("answers 409 no_free_line for a city without a free line, even when the country has one", async () => {
    const here = freshCountry();
    await lineAt({ country: here, city: "Alpha" });

    const response = await createIn((await verifiedMember()).sessionToken, { country: here, city: "Beta" });

    expect(response.status).toBe(409);
    expect(await expectContract("POST", "/v1/wireguard/peers", response)).toMatchObject({ error: "no_free_line" });
  });

  it("never overfills a line when members ask for the same region at once", async () => {
    const here = freshCountry();
    await lineAt({ country: here, city: "Alpha" });
    const members: Member[] = [];
    for (let i = 0; i <= MAX_USERS_PER_LINE; i++) members.push(await verifiedMember());

    const responses = await Promise.all(members.map((m) => createIn(m.sessionToken, { country: here })));

    expect(responses.map((r) => r.status).sort()).toEqual([...Array(MAX_USERS_PER_LINE).fill(201), 409]);
  });

  it.each<{ why: string; region: Record<string, unknown> }>([
    { why: "line_id and country together", region: { country: "DE", line_id: crypto.randomUUID() } },
    { why: "a city without a country", region: { city: "Berlin" } },
    { why: "neither line_id nor country", region: {} },
    { why: "a lowercase country", region: { country: "de" } },
    { why: "an empty city", region: { country: "DE", city: "" } },
    { why: "a city that is not a string", region: { country: "DE", city: 7 } },
  ])("rejects $why with 400", async ({ region }) => {
    const response = await createIn((await verifiedMember()).sessionToken, region);

    expect(response.status).toBe(400);
    expect(await expectContract("POST", "/v1/wireguard/peers", response)).toMatchObject({ error: "invalid_request" });
  });
});

describe("a peer's line", () => {
  it("is listed with each peer, also for a peer created with a line_id", async () => {
    const here = freshCountry();
    const lineId = await lineAt({ country: here, city: "Alpha", isp: "Vodafone GmbH" });
    const member = await verifiedMember();
    await peerOn(lineId, member);

    const response = await send("GET", "/v1/wireguard/peers", member.sessionToken);
    const { peers } = await expectContract<{ peers: Peer[] }>("GET", "/v1/wireguard/peers", response);

    expect(peers.map((p) => p.line)).toEqual([{ country: here, city: "Alpha", isp: "Vodafone GmbH" }]);
  });
});

describe("PATCH /v1/wireguard/peers/{peer_id} with a country", () => {
  function move(sessionToken: string, peerId: string, body: Record<string, unknown>) {
    return send("PATCH", `/v1/wireguard/peers/${peerId}`, sessionToken, body);
  }

  it("moves a peer to another free line in the city, on its own gateway", async () => {
    const here = freshCountry();
    const member = await verifiedMember();
    const first = await lineAt({ country: here, city: "Alpha" });
    const { peer } = await createdIn(member.sessionToken, { country: here, city: "Alpha" });
    expect(peer.line_id).toBe(first);
    const second = await lineAt({ country: here, city: "Alpha", isp: "M-net" });
    await lineAt({ country: here, city: "Beta" });

    const response = await move(member.sessionToken, peer.peer_id, { country: here, city: "Alpha" });

    expect(response.status).toBe(200);
    const moved = await expectContract<Peer & { failover: string }>("PATCH", "/v1/wireguard/peers/{peer_id}", response);
    expect(moved.line_id).toBe(second);
    expect(moved.line).toEqual({ country: here, city: "Alpha", isp: "M-net" });
    expect(moved.failover).toBe("country");
  });

  it("moves anywhere in the country without a city, and changes the failover in the same request", async () => {
    const here = freshCountry();
    const member = await verifiedMember();
    await lineAt({ country: here, city: "Alpha" });
    const { peer } = await createdIn(member.sessionToken, { country: here });
    const beta = await lineAt({ country: here, city: "Beta" });

    const response = await move(member.sessionToken, peer.peer_id, { country: here, failover: "city" });

    const moved = await expectContract<Peer & { failover: string }>("PATCH", "/v1/wireguard/peers/{peer_id}", response);
    expect(moved.line_id).toBe(beta);
    expect(moved.failover).toBe("city");
  });

  it("never moves a peer to a line on another gateway, which its config doesn't name", async () => {
    const here = freshCountry();
    const member = await verifiedMember();
    await lineAt({ country: here, city: "Alpha" });
    const { peer } = await createdIn(member.sessionToken, { country: here });
    const elsewhere = await announcedGateway();
    await onlineLine(elsewhere, { country: here, city: "Alpha" });

    const response = await move(member.sessionToken, peer.peer_id, { country: here });

    expect(response.status).toBe(409);
    expect(await expectContract("PATCH", "/v1/wireguard/peers/{peer_id}", response)).toMatchObject({ error: "no_free_line" });
    expect((await (await send("GET", "/v1/wireguard/peers", member.sessionToken)).json<{ peers: Peer[] }>()).peers[0]?.line_id).toBe(peer.line_id);
  });

  it("answers 409 no_free_line, and keeps the failover unchanged, when the region has no other free line", async () => {
    const here = freshCountry();
    const member = await verifiedMember();
    await lineAt({ country: here, city: "Alpha" });
    const { peer } = await createdIn(member.sessionToken, { country: here });

    const response = await move(member.sessionToken, peer.peer_id, { country: here, failover: "off" });

    expect(response.status).toBe(409);
    expect(await expectContract("PATCH", "/v1/wireguard/peers/{peer_id}", response)).toMatchObject({ error: "no_free_line" });
    const listed = (await (await send("GET", "/v1/wireguard/peers", member.sessionToken)).json<{ peers: { failover: string }[] }>()).peers;
    expect(listed[0]?.failover).toBe("country");
  });

  it.each<{ why: string; body: Record<string, unknown> }>([
    { why: "line_id and country together", body: { line_id: crypto.randomUUID(), country: "DE" } },
    { why: "a city without a country", body: { city: "Berlin" } },
    { why: "a lowercase country", body: { country: "de" } },
    { why: "an empty city", body: { country: "DE", city: "" } },
  ])("rejects $why with 400", async ({ body }) => {
    const member = await verifiedMember();
    const here = freshCountry();
    await lineAt({ country: here, city: "Alpha" });
    const { peer } = await createdIn(member.sessionToken, { country: here });

    const response = await move(member.sessionToken, peer.peer_id, body);

    expect(response.status).toBe(400);
    expect(await expectContract("PATCH", "/v1/wireguard/peers/{peer_id}", response)).toMatchObject({ error: "invalid_request" });
  });
});
