import { createExecutionContext, createScheduledController, waitOnExecutionContext } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { beforeAll, describe, expect, it } from "vitest";
import worker from "../src/index";
import { RECOUNT_CRON, RECOUNT_STALE_MS } from "../src/lines/regions";
import { expectContract } from "./contract";
import { MAX_USERS_PER_LINE } from "../src/pricing";
import { announcedGateway, peerOn } from "./gateway-fixtures";
import { signIn, uniqueEmail } from "./helpers";
import { countryPool, type Member, onlineLine as onlineLineAt } from "./region-fixtures";
import { link, send, sharingLine, verifiedMember } from "./share-fixtures";

interface CityRegion {
  city: string;
  lines: number;
}

interface CountryRegion {
  country: string;
  lines: number;
  cities?: CityRegion[];
}

let gateway: { gatewayId: string; token: string };

beforeAll(async () => {
  gateway = await announcedGateway();
});

const freshCountry = countryPool(["QM", "QN", "QO", "QP", "QQ", "QR", "QS", "QT", "QU", "QV", "QW", "QY", "QZ", "XA", "XB", "XC"]);

function onlineLine(country: string, city?: string, owner?: Member): Promise<string> {
  return onlineLineAt(gateway, { country, ...(city ? { city } : {}) }, owner);
}

/** Runs the cron trigger that recounts the free lines in each region. */
async function recount(scheduledTime = Date.now()): Promise<void> {
  const ctx = createExecutionContext();
  worker.scheduled(createScheduledController({ scheduledTime, cron: RECOUNT_CRON }), env, ctx);
  await waitOnExecutionContext(ctx);
}

async function regions(sessionToken: string, country?: string, peerId?: string): Promise<CountryRegion[]> {
  const query = new URLSearchParams({ ...(country ? { country } : {}), ...(peerId ? { peer_id: peerId } : {}) });
  const response = await send("GET", `/v1/lines/regions${query.size ? `?${query.toString()}` : ""}`, sessionToken);
  expect(response.status).toBe(200);
  return (await expectContract<{ countries: CountryRegion[] }>("GET", "/v1/lines/regions", response)).countries;
}

describe("GET /v1/lines/regions", () => {
  it("counts the free, online lines in each country", async () => {
    const [here, there] = [freshCountry(), freshCountry()];
    await onlineLine(here, "Alpha");
    await onlineLine(here, "Beta");
    await onlineLine(there);

    await recount();
    const countries = await regions((await verifiedMember()).sessionToken);

    expect(countries).toContainEqual({ country: here, lines: 2 });
    expect(countries).toContainEqual({ country: there, lines: 1 });
  });

  it("lists a country's cities, most lines first, when asked for that country", async () => {
    const [here, elsewhere] = [freshCountry(), freshCountry()];
    await onlineLine(here, "Beta");
    await onlineLine(here, "Alpha");
    await onlineLine(here, "Gamma");
    await onlineLine(here, "Gamma");
    await onlineLine(here);
    await onlineLine(elsewhere, "Delta");

    await recount();
    const countries = await regions((await verifiedMember()).sessionToken, here);

    expect(countries).toEqual([
      {
        country: here,
        lines: 5,
        cities: [
          { city: "Gamma", lines: 2 },
          { city: "Alpha", lines: 1 },
          { city: "Beta", lines: 1 },
        ],
      },
    ]);
  });

  it("returns no countries for a country without free lines", async () => {
    await recount();
    expect(await regions((await verifiedMember()).sessionToken, freshCountry())).toEqual([]);
  });

  it("counts only the lines the member could get: online, served, not linked, with room", async () => {
    const here = freshCountry();
    const member = await verifiedMember();
    await onlineLine(here, "Alpha");
    // Not counted: offline, the member's own, linked, owner suspended or unverified, full.
    await sharingLine((await verifiedMember()).sessionToken, { country: here, city: "Alpha" } as Partial<IncomingRequestCfProperties>);
    await onlineLine(here, "Alpha", member);
    const household = await verifiedMember();
    await link(member.memberId, household.memberId, "household");
    await onlineLine(here, "Alpha", household);
    const suspended = await verifiedMember();
    await onlineLine(here, "Alpha", suspended);
    await env.DB.prepare(`UPDATE members SET status = 'suspended' WHERE id = ?`).bind(suspended.memberId).run();
    const lapsed = await verifiedMember();
    await onlineLine(here, "Alpha", lapsed);
    await env.DB.prepare(`UPDATE members SET identity_status = 'unverified' WHERE id = ?`).bind(lapsed.memberId).run();
    const full = await onlineLine(here, "Alpha");
    for (let i = 0; i < MAX_USERS_PER_LINE; i++) await peerOn(full);
    await recount();

    expect(await regions(member.sessionToken, here)).toEqual([
      { country: here, lines: 1, cities: [{ city: "Alpha", lines: 1 }] },
    ]);
    expect(await regions(member.sessionToken)).toContainEqual({ country: here, lines: 1 });
  });

  it("still counts a full line the member already uses", async () => {
    const here = freshCountry();
    const member = await verifiedMember();
    const mine = await onlineLine(here, "Alpha");
    await peerOn(mine, member);
    for (let i = 1; i < MAX_USERS_PER_LINE; i++) await peerOn(mine);
    await recount();

    expect(await regions(member.sessionToken, here)).toEqual([
      { country: here, lines: 1, cities: [{ city: "Alpha", lines: 1 }] },
    ]);
  });

  it("counts a line that came online from the next recount, a minute later at most", async () => {
    const here = freshCountry();
    await recount();
    await onlineLine(here, "Alpha");
    const member = await verifiedMember();

    const before = await regions(member.sessionToken, here);
    await recount();
    const after = await regions(member.sessionToken, here);

    expect(before).toEqual([]);
    expect(after).toEqual([{ country: here, lines: 1, cities: [{ city: "Alpha", lines: 1 }] }]);
  });

  it("recounts on request when no recount ran for a while, as in local development", async () => {
    const here = freshCountry();
    await recount(Date.now() - RECOUNT_STALE_MS - 1_000);
    await onlineLine(here, "Alpha");

    expect(await regions((await verifiedMember()).sessionToken, here)).toEqual([
      { country: here, lines: 1, cities: [{ city: "Alpha", lines: 1 }] },
    ]);
  });

  it("leaves out a line linked to the member at once, without waiting for a recount", async () => {
    const here = freshCountry();
    const member = await verifiedMember();
    const owner = await verifiedMember();
    await onlineLine(here, "Alpha", owner);
    await onlineLine(here, "Beta");
    await recount();

    await link(owner.memberId, member.memberId, "card");

    expect(await regions(member.sessionToken, here)).toEqual([
      { country: here, lines: 1, cities: [{ city: "Beta", lines: 1 }] },
    ]);
  });

  it("counts lines whose city is empty and lines whose city is unknown together, for the country only", async () => {
    const here = freshCountry();
    await onlineLine(here, "Alpha");
    await onlineLineAt(gateway, { country: here, city: "" });
    await onlineLine(here);
    await recount();

    expect(await regions((await verifiedMember()).sessionToken, here)).toEqual([
      { country: here, lines: 3, cities: [{ city: "Alpha", lines: 1 }] },
    ]);
  });

  it("recounts on request only once per stale spell, however many requests find the counts stale", async () => {
    const here = freshCountry();
    await recount(Date.now() - RECOUNT_STALE_MS - 1_000);
    const member = await verifiedMember();
    await regions(member.sessionToken, here);
    await onlineLine(here, "Alpha");

    expect(await regions(member.sessionToken, here)).toEqual([]);
  });

  it("leaves out the line of the member's own device named by peer_id, as when moving it", async () => {
    const here = freshCountry();
    const member = await verifiedMember();
    const line = await onlineLine(here, "Alpha");
    const { peer } = await peerOn(line, member);
    const other = await verifiedMember();
    const { peer: theirs } = await peerOn(await onlineLine(here, "Beta"), other);
    await recount();

    const mine = await regions(member.sessionToken, here, peer.peer_id);
    const notMine = await regions(member.sessionToken, here, theirs.peer_id);
    const countries = await regions(member.sessionToken, undefined, peer.peer_id);

    expect(mine).toEqual([{ country: here, lines: 1, cities: [{ city: "Beta", lines: 1 }] }]);
    expect(notMine).toEqual([
      { country: here, lines: 2, cities: [{ city: "Alpha", lines: 1 }, { city: "Beta", lines: 1 }] },
    ]);
    expect(countries).toContainEqual({ country: here, lines: 1 });
  });

  it.each(["country=de", "country=DEU", "country=D1", "country=", "peer_id=nope"])("rejects %s with 400", async (query) => {
    const response = await send("GET", `/v1/lines/regions?${query}`, (await verifiedMember()).sessionToken);

    expect(response.status).toBe(400);
    expect(await expectContract("GET", "/v1/lines/regions", response)).toMatchObject({ error: "invalid_request" });
  });

  it("returns 403 identity_required before the member is verified", async () => {
    const response = await send("GET", "/v1/lines/regions", (await signIn(uniqueEmail())).sessionToken);

    expect(response.status).toBe(403);
    expect(await expectContract("GET", "/v1/lines/regions", response)).toEqual({ error: "identity_required" });
  });
});
