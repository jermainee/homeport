import { env } from "cloudflare:workers";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LEASE_CREDITS } from "../src/pricing";
import { expectContract } from "./contract";
import {
  type UsageEventInput,
  announcedGateway,
  balancesOf,
  failoverPeer,
  gatewayCall,
  grantedLease,
  SESSION_START,
  reportUsage,
  servedPeer,
  usageEvent,
} from "./gateway-fixtures";
import { uuidv7 } from "./helpers";
import { driftedBalances, platformBalance, unbalancedTransactions } from "./ledger-fixtures";
import { send } from "./share-fixtures";

interface Results {
  results: { event_id: string; status: string; reason?: string }[];
}

/** A served peer with a lease, and a builder for its session's events. */
async function session(credits = 1_000_000) {
  const served = await servedPeer(credits);
  const lease = await grantedLease(served.gateway.token, served.peer.peer_id);
  const event = (input: Omit<UsageEventInput, "sessionId" | "peerId" | "lineId" | "leaseId"> & Partial<UsageEventInput>) =>
    usageEvent({
      sessionId: lease.sessionId,
      peerId: served.peer.peer_id,
      lineId: served.lineId,
      leaseId: lease.lease_id,
      ...input,
    });
  return { ...served, lease, event };
}

async function report(token: string, events: unknown[]): Promise<Results> {
  const response = await reportUsage(token, events);
  expect(response.status).toBe(200);
  return expectContract<Results>("POST", "/v1/gateway/usage", response);
}

function statuses(results: Results) {
  return results.results.map((result) => (result.reason ? `${result.status}:${result.reason}` : result.status));
}

describe("POST /v1/gateway/usage", () => {
  it("charges the user 10 credits per MB, pays the owner 5 and keeps 5 for the platform", async () => {
    const { gateway, owner, user, event } = await session();
    const platformBefore = await platformBalance("platform_revenue");

    const results = await report(gateway.token, [event({ seq: 0, bytesUp: 1_000_000, bytesDown: 9_000_000 })]);

    expect(statuses(results)).toEqual(["applied"]);
    expect((await balancesOf(user.memberId)).purchased).toBe(1_000_000 - 100);
    expect((await balancesOf(owner.memberId)).earned_pending).toBe(50);
    expect((await platformBalance("platform_revenue")) - platformBefore).toBe(50);
    expect(await unbalancedTransactions()).toEqual([]);
    expect(await driftedBalances()).toEqual([]);
  });

  it("rounds the meter's running total down, so rounding never adds up across events", async () => {
    const { gateway, owner, user, event } = await session();

    await report(gateway.token, [
      event({ seq: 0, bytesUp: 0, bytesDown: 150_000 }),
      event({ seq: 1, bytesUp: 0, bytesDown: 150_000 }),
      event({ seq: 2, bytesUp: 0, bytesDown: 150_000 }),
    ]);

    // 450,000 bytes: floor(4.5) = 4 credits charged, floor(2.25) = 2 earned.
    expect((await balancesOf(user.memberId)).purchased).toBe(1_000_000 - 4);
    expect((await balancesOf(owner.memberId)).earned_pending).toBe(2);
  });

  it("answers a retried event duplicate and does not count it again, in the same batch or a later one", async () => {
    const { gateway, user, event } = await session();
    const first = event({ seq: 0, bytesUp: 0, bytesDown: 5_000_000 });

    const once = await report(gateway.token, [first, first]);
    const later = await report(gateway.token, [first]);

    expect(statuses(once)).toEqual(["applied", "duplicate"]);
    expect(statuses(later)).toEqual(["duplicate"]);
    expect((await balancesOf(user.memberId)).purchased).toBe(1_000_000 - 50);
  });

  it("rejects an event_id reused for other bytes", async () => {
    const { gateway, event } = await session();
    const first = event({ seq: 0, bytesUp: 0, bytesDown: 5_000_000 });
    await report(gateway.token, [first]);

    const results = await report(gateway.token, [{ ...first, bytes_down: 6_000_000 }]);

    expect(statuses(results)).toEqual(["rejected:seq_conflict"]);
  });

  it("answers each event in request order and keeps going past rejected ones", async () => {
    const { gateway, user, event } = await session();

    const results = await report(gateway.token, [
      event({ seq: 0, bytesUp: 0, bytesDown: 1_000_000 }),
      event({ seq: 1, bytesUp: 0, bytesDown: 1_000_000, leaseId: crypto.randomUUID() }),
      event({ seq: 1, bytesUp: 0, bytesDown: 1_000_000 }),
    ]);

    expect(statuses(results)).toEqual(["applied", "rejected:unknown_lease", "applied"]);
    expect(results.results.map((result) => result.event_id)).toHaveLength(3);
    expect((await balancesOf(user.memberId)).purchased).toBe(1_000_000 - 20);
  });

  it.each([
    ["unknown_peer", { peerId: "0000000a-0000-4000-8000-000000000000" }],
    ["unknown_line", { lineId: "0000000a-0000-4000-8000-000000000000" }],
    ["unknown_lease", { leaseId: "0000000a-0000-4000-8000-000000000000" }],
    ["lease_mismatch", { sessionId: uuidv7() }],
    ["seq_conflict", { seq: 1 }],
    ["expired", { eventId: uuidv7(Date.now() - 73 * 60 * 60 * 1000) }],
  ] as [string, Partial<UsageEventInput>][])("rejects %s and charges nothing", async (reason, change) => {
    const { gateway, user, event } = await session();

    const results = await report(gateway.token, [event({ seq: 0, bytesUp: 0, bytesDown: 1_000_000, ...change })]);

    expect(statuses(results)).toEqual([`rejected:${reason}`]);
    expect((await balancesOf(user.memberId)).purchased).toBe(1_000_000);
  });

  it("rejects a gap or repeat in seq, and events after the final one", async () => {
    const { gateway, event } = await session();
    await report(gateway.token, [event({ seq: 0, bytesUp: 0, bytesDown: 1 })]);

    const results = await report(gateway.token, [
      event({ seq: 0, bytesUp: 0, bytesDown: 1 }),
      event({ seq: 2, bytesUp: 0, bytesDown: 1 }),
      event({ seq: 1, bytesUp: 0, bytesDown: 1, final: true }),
      event({ seq: 2, bytesUp: 0, bytesDown: 1 }),
    ]);

    expect(statuses(results)).toEqual(["rejected:seq_conflict", "rejected:seq_conflict", "applied", "rejected:seq_conflict"]);
  });

  it("rejects a period that does not start where the last one ended, or ends before it starts", async () => {
    const { gateway, event } = await session();
    await report(gateway.token, [event({ seq: 0, bytesUp: 0, bytesDown: 1 })]);
    const backwards = event({ seq: 1, bytesUp: 0, bytesDown: 1 });

    const results = await report(gateway.token, [
      event({ seq: 1, bytesUp: 0, bytesDown: 1, start: 45 }),
      { ...backwards, period_end: backwards.period_start },
    ]);

    expect(statuses(results)).toEqual(["rejected:invalid_period", "rejected:invalid_period"]);
  });

  it("rejects another gateway's lease as unknown", async () => {
    const { event } = await session();
    const other = await announcedGateway();

    const results = await report(other.token, [event({ seq: 0, bytesUp: 0, bytesDown: 1 })]);

    expect(statuses(results)).toEqual(["rejected:unknown_lease"]);
  });

  it("charges the actual bytes past the lease and after it expired, as the gateway is the meter of record", async () => {
    const { gateway, user, event } = await session(LEASE_CREDITS);

    const results = await report(gateway.token, [event({ seq: 0, bytesUp: 0, bytesDown: 250_000_000 })]);

    expect(statuses(results)).toEqual(["applied"]);
    expect((await balancesOf(user.memberId)).purchased).toBe(LEASE_CREDITS - 2_500);
  });

  it("still charges a deleted peer's late usage", async () => {
    const { gateway, user, peer, event } = await session();
    await send("DELETE", `/v1/wireguard/peers/${peer.peer_id}`, user.sessionToken);

    const results = await report(gateway.token, [event({ seq: 0, bytesUp: 0, bytesDown: 1_000_000 })]);

    expect(statuses(results)).toEqual(["applied"]);
    expect((await balancesOf(user.memberId)).purchased).toBe(1_000_000 - 10);
  });

  it("charges usage against the lease, so returning it releases only the rest", async () => {
    const { gateway, lease, event } = await session();
    await report(gateway.token, [event({ seq: 0, bytesUp: 0, bytesDown: 50_000_000, final: true })]);

    const response = await gatewayCall(gateway.token, "POST", `/v1/gateway/leases/${lease.lease_id}/return`, { request_id: uuidv7() });

    expect(await response.json()).toEqual({ released_credits: LEASE_CREDITS - 500 });
  });

  it("logs each session's metadata, and nothing about destinations", async () => {
    const { gateway, owner, user, lineId, peer, lease, event } = await session();

    await report(gateway.token, [
      event({ seq: 0, bytesUp: 1_000, bytesDown: 2_000 }),
      event({ seq: 1, bytesUp: 3_000, bytesDown: 4_000, final: true }),
    ]);

    const logged = await env.DB.prepare(`SELECT * FROM usage_sessions WHERE session_id = ?`).bind(lease.sessionId).first();
    const start = event({ seq: 0, bytesUp: 0, bytesDown: 0 });
    expect(logged).toEqual({
      session_id: lease.sessionId,
      gateway_id: gateway.gatewayId,
      peer_id: peer.peer_id,
      member_id: user.memberId,
      line_id: lineId,
      owner_member_id: owner.memberId,
      exit_ip: "203.0.113.47",
      started_at: Date.parse(start.period_start),
      last_period_end: Date.parse(start.period_start) + 60_000,
      next_seq: 2,
      bytes_up: 4_000,
      bytes_down: 6_000,
      ended: 1,
      updated_at: expect.any(Number),
    });
  });

  it("answers a batch sent again while the first is still running without rejecting what the first applied", async () => {
    const { gateway, user, event } = await session();
    const events = [event({ seq: 0, bytesUp: 0, bytesDown: 1_000_000 }), event({ seq: 1, bytesUp: 0, bytesDown: 2_000_000 })];

    const [first, second] = await Promise.all([report(gateway.token, events), report(gateway.token, events)]);

    expect([...statuses(first), ...statuses(second)].filter((status) => status.startsWith("rejected"))).toEqual([]);
    expect((await balancesOf(user.memberId)).purchased).toBe(1_000_000 - 30);
  });

  it("rejects a batch that does not match the contract with 400", async () => {
    const { gateway, event } = await session();
    const valid = event({ seq: 0, bytesUp: 0, bytesDown: 1 });
    const withoutStart: Record<string, unknown> = { ...valid };
    delete withoutStart["session_start"];

    const bodies = [
      { events: [] },
      { events: Array.from({ length: 101 }, () => valid) },
      { events: [{ ...valid, destination: "203.0.113.9:443" }] },
      { events: [withoutStart] },
      { events: [{ ...valid, bytes_up: -1 }] },
      // Each count is a safe integer, but their sum is not.
      { events: [{ ...valid, bytes_up: 2 ** 52, bytes_down: 2 ** 52 }] },
      { events: [valid], extra: true },
    ];

    for (const body of bodies) {
      const response = await gatewayCall(gateway.token, "POST", "/v1/gateway/usage", body);
      expect(response.status).toBe(400);
      expect(await expectContract("POST", "/v1/gateway/usage", response)).toMatchObject({ error: "invalid_request" });
    }
  });
});

describe("POST /v1/gateway/usage on a fallback line", () => {
  async function sessionRows(peerId: string) {
    const { results } = await env.DB.prepare(
      `SELECT line_id, owner_member_id, bytes_down FROM usage_sessions WHERE peer_id = ? ORDER BY started_at, rowid`,
    )
      .bind(peerId)
      .all<{ line_id: string; owner_member_id: string; bytes_down: number }>();
    return results;
  }

  it("pays the owner of the line each session exits through", async () => {
    const { gateway, primary, fallback, user, peer } = await failoverPeer();
    const onPrimary = await grantedLease(gateway.token, peer.peer_id);
    const onFallback = await grantedLease(gateway.token, peer.peer_id, uuidv7(), fallback.lineId);

    const results = await report(gateway.token, [
      usageEvent({
        sessionId: onPrimary.sessionId,
        peerId: peer.peer_id,
        lineId: primary.lineId,
        leaseId: onPrimary.lease_id,
        seq: 0,
        bytesUp: 0,
        bytesDown: 2_000_000,
        final: true,
      }),
      usageEvent({
        sessionId: onFallback.sessionId,
        peerId: peer.peer_id,
        lineId: fallback.lineId,
        leaseId: onFallback.lease_id,
        seq: 0,
        bytesUp: 0,
        bytesDown: 4_000_000,
      }),
    ]);

    expect(statuses(results)).toEqual(["applied", "applied"]);
    expect((await balancesOf(user.memberId)).purchased).toBe(1_000_000 - 60);
    expect((await balancesOf(primary.owner.memberId)).earned_pending).toBe(10);
    expect((await balancesOf(fallback.owner.memberId)).earned_pending).toBe(20);
    expect(await sessionRows(peer.peer_id)).toEqual([
      { line_id: primary.lineId, owner_member_id: primary.owner.memberId, bytes_down: 2_000_000 },
      { line_id: fallback.lineId, owner_member_id: fallback.owner.memberId, bytes_down: 4_000_000 },
    ]);
    expect(await unbalancedTransactions()).toEqual([]);
  });

  it("rejects an event on another line than its lease's", async () => {
    const { gateway, primary, fallback, peer } = await failoverPeer();
    const onFallback = await grantedLease(gateway.token, peer.peer_id, uuidv7(), fallback.lineId);

    const results = await report(gateway.token, [
      usageEvent({
        sessionId: onFallback.sessionId,
        peerId: peer.peer_id,
        lineId: primary.lineId,
        leaseId: onFallback.lease_id,
        seq: 0,
        bytesUp: 0,
        bytesDown: 1_000_000,
      }),
    ]);

    expect(statuses(results)).toEqual(["rejected:lease_mismatch"]);
  });

  it("pays the old line's owner for a session that started before the peer moved", async () => {
    const { gateway, primary, fallback, user, peer } = await failoverPeer();
    const lease = await grantedLease(gateway.token, peer.peer_id);
    const event = (seq: number) =>
      usageEvent({ sessionId: lease.sessionId, peerId: peer.peer_id, lineId: primary.lineId, leaseId: lease.lease_id, seq, bytesUp: 0, bytesDown: 2_000_000 });
    await report(gateway.token, [event(0)]);

    await send("PATCH", `/v1/wireguard/peers/${peer.peer_id}`, user.sessionToken, { line_id: fallback.lineId });
    const results = await report(gateway.token, [event(1)]);

    expect(statuses(results)).toEqual(["applied"]);
    expect((await balancesOf(primary.owner.memberId)).earned_pending).toBe(20);
    expect((await balancesOf(fallback.owner.memberId)).earned_pending).toBe(0);
  });

  it("shows the member which line a device exits through, until its session ends or the gateway stops reporting it", async () => {
    vi.useFakeTimers({ toFake: ["Date"], now: Date.now() });
    const { gateway, fallback, user, peer } = await failoverPeer();
    const lease = await grantedLease(gateway.token, peer.peer_id, uuidv7(), fallback.lineId);
    const event = (seq: number, final = false) =>
      usageEvent({ sessionId: lease.sessionId, peerId: peer.peer_id, lineId: fallback.lineId, leaseId: lease.lease_id, seq, bytesUp: 0, bytesDown: 1_000, final });
    const exitLine = async () => {
      const response = await send("GET", "/v1/wireguard/peers", user.sessionToken);
      const { peers } = await expectContract<{ peers: { peer_id: string; exit_line: unknown }[] }>("GET", "/v1/wireguard/peers", response);
      return peers.find((listed) => listed.peer_id === peer.peer_id)?.exit_line;
    };

    const before = await exitLine();
    await report(gateway.token, [event(0)]);
    const during = await exitLine();
    vi.setSystemTime(Date.now() + 3 * 60_000 + 1_000);
    const stale = await exitLine();
    await report(gateway.token, [event(1, true)]);
    const ended = await exitLine();

    expect(before).toBeNull();
    expect(during).toEqual({ line_id: fallback.lineId, country: "DE", city: "Berlin", since: new Date(SESSION_START).toISOString().replace(".000Z", "Z") });
    expect(stale).toBeNull();
    expect(ended).toBeNull();
  });
});

afterEach(() => {
  vi.useRealTimers();
});
