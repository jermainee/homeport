import { env } from "cloudflare:workers";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LEASE_BYTES, LEASE_CREDITS, LEASE_GRACE_SECONDS, LEASE_RENEW_AT_BYTES, LEASE_TTL_SECONDS } from "../src/pricing";
import { expectContract } from "./contract";
import {
  type LeaseBody,
  announcedGateway,
  gatewayCall,
  giveCredits,
  grantedLease,
  peerOn,
  requestLease,
  servedPeer,
} from "./gateway-fixtures";
import { uuidv7 } from "./helpers";
import { link, send } from "./share-fixtures";

async function reservations(memberId: string) {
  const { results } = await env.DB.prepare(
    `SELECT id, credits, expires_at, released_at FROM credit_reservations WHERE member_id = ? ORDER BY created_at`,
  )
    .bind(memberId)
    .all<{ id: string; credits: number; expires_at: number; released_at: number | null }>();
  return results;
}

function returnLease(token: string, leaseId: string, requestId = uuidv7()) {
  return gatewayCall(token, "POST", `/v1/gateway/leases/${leaseId}/return`, { request_id: requestId });
}

afterEach(() => {
  vi.useRealTimers();
});

describe("POST /v1/gateway/leases", () => {
  it("grants a lease in bytes and reserves its credits from the member", async () => {
    const { gateway, user, peer } = await servedPeer();
    const before = Date.now();

    const response = await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: uuidv7() });

    expect(response.status).toBe(201);
    const lease = await expectContract<LeaseBody>("POST", "/v1/gateway/leases", response);
    expect(lease).toMatchObject({ granted_bytes: LEASE_BYTES, renew_at_bytes: LEASE_RENEW_AT_BYTES, grace_seconds: LEASE_GRACE_SECONDS });
    expect([LEASE_BYTES, LEASE_RENEW_AT_BYTES]).toEqual([200_000_000, 160_000_000]);
    const expiresAt = Date.parse(lease.expires_at);
    expect(expiresAt).toBeGreaterThanOrEqual(before + LEASE_TTL_SECONDS * 1000 - 1000);
    expect(expiresAt).toBeLessThanOrEqual(Date.now() + LEASE_TTL_SECONDS * 1000);
    expect(await reservations(user.memberId)).toEqual([
      { id: lease.lease_id, credits: LEASE_CREDITS, expires_at: expiresAt, released_at: null },
    ]);
  });

  it("grants a smaller last lease for the credits that are left", async () => {
    const { gateway, user, peer } = await servedPeer(500);

    const response = await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: uuidv7() });

    expect(response.status).toBe(201);
    const lease = await expectContract<LeaseBody>("POST", "/v1/gateway/leases", response);
    // 500 credits at 10 per MB.
    expect(lease).toMatchObject({ granted_bytes: 50_000_000, renew_at_bytes: 40_000_000 });
    expect((await reservations(user.memberId)).map((r) => r.credits)).toEqual([500]);
  });

  it("answers 402 with the grace period when the member has no credits left", async () => {
    const { gateway, peer } = await servedPeer(0);

    const response = await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: uuidv7() });

    expect(response.status).toBe(402);
    expect(await expectContract("POST", "/v1/gateway/leases", response)).toEqual({
      error: "insufficient_credits",
      grace_seconds: LEASE_GRACE_SECONDS,
    });
  });

  it("counts credits that open leases hold as spent", async () => {
    const { gateway, peer } = await servedPeer(LEASE_CREDITS);
    await grantedLease(gateway.token, peer.peer_id);

    const second = await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: uuidv7() });

    expect(second.status).toBe(402);
  });

  it("answers a retry with the same request_id with the same lease and reserves once", async () => {
    const { gateway, user, peer } = await servedPeer();
    const input = { peerId: peer.peer_id, sessionId: uuidv7(), requestId: uuidv7() };

    const first = await (await requestLease(gateway.token, input)).json();
    const retry = await requestLease(gateway.token, input);

    expect(retry.status).toBe(201);
    expect(await retry.json()).toEqual(first);
    expect(await reservations(user.memberId)).toHaveLength(1);
  });

  it("answers 409 to a request_id reused with another body", async () => {
    const { gateway, peer } = await servedPeer();
    const requestId = uuidv7();
    await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: uuidv7(), requestId });

    const reused = await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: uuidv7(), requestId });

    expect(reused.status).toBe(409);
    await expectContract("POST", "/v1/gateway/leases", reused);
  });

  it("answers 409 when another gateway reuses a request_id", async () => {
    const { gateway, peer } = await servedPeer();
    const other = await announcedGateway();
    const body = { request_id: uuidv7(), peer_id: peer.peer_id, session_id: uuidv7() };
    await gatewayCall(gateway.token, "POST", "/v1/gateway/leases", body);

    const response = await gatewayCall(other.token, "POST", "/v1/gateway/leases", body);

    expect(response.status).toBe(409);
  });

  it("rejects an unknown field and a malformed id with 400", async () => {
    const { gateway, peer } = await servedPeer();

    const unknown = await gatewayCall(gateway.token, "POST", "/v1/gateway/leases", {
      request_id: uuidv7(),
      peer_id: peer.peer_id,
      session_id: uuidv7(),
      bytes: 1,
    });
    const malformed = await gatewayCall(gateway.token, "POST", "/v1/gateway/leases", {
      request_id: uuidv7(),
      peer_id: peer.peer_id,
      session_id: "not-a-uuid",
    });

    expect([unknown.status, malformed.status]).toEqual([400, 400]);
    await expectContract("POST", "/v1/gateway/leases", unknown);
  });

  describe("403 peer_inactive", () => {
    async function expectInactive(token: string, peerId: string) {
      const response = await requestLease(token, { peerId, sessionId: uuidv7() });
      expect(response.status).toBe(403);
      expect(await expectContract("POST", "/v1/gateway/leases", response)).toEqual({ error: "peer_inactive" });
    }

    it("for an unknown peer", async () => {
      const { token } = await announcedGateway();
      await expectInactive(token, crypto.randomUUID());
    });

    it("for a deleted peer", async () => {
      const { gateway, user, peer } = await servedPeer();
      await send("DELETE", `/v1/wireguard/peers/${peer.peer_id}`, user.sessionToken);
      await expectInactive(gateway.token, peer.peer_id);
    });

    it("for another gateway's peer", async () => {
      const { peer } = await servedPeer();
      const other = await announcedGateway();
      await expectInactive(other.token, peer.peer_id);
    });

    it("for a suspended member", async () => {
      const { gateway, user, peer } = await servedPeer();
      await env.DB.prepare(`UPDATE members SET status = 'suspended' WHERE id = ?`).bind(user.memberId).run();
      await expectInactive(gateway.token, peer.peer_id);
    });

    it("for a line whose sharing is off", async () => {
      const { gateway, owner, lineId, peer } = await servedPeer();
      await send("PATCH", `/v1/lines/${lineId}`, owner.sessionToken, { sharing_enabled: false });
      await expectInactive(gateway.token, peer.peer_id);
    });

    it("for a member linked to the line's owner", async () => {
      const { gateway, owner, user, peer } = await servedPeer();
      await link(owner.memberId, user.memberId, "card");
      await expectInactive(gateway.token, peer.peer_id);
    });
  });

  describe("renewal (supersedes_lease_id)", () => {
    it("grants the next lease and reserves for it too", async () => {
      const { gateway, user, peer } = await servedPeer();
      const current = await grantedLease(gateway.token, peer.peer_id);

      const response = await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: current.sessionId, supersedes: current.lease_id });

      expect(response.status).toBe(201);
      const next = await expectContract<LeaseBody>("POST", "/v1/gateway/leases", response);
      expect(next.lease_id).not.toBe(current.lease_id);
      expect((await reservations(user.memberId)).map((r) => r.id)).toEqual([current.lease_id, next.lease_id]);
    });

    it("returns the existing successor with 200 when the lease was already superseded", async () => {
      const { gateway, user, peer } = await servedPeer();
      const current = await grantedLease(gateway.token, peer.peer_id);
      const renewal = { peerId: peer.peer_id, sessionId: current.sessionId, supersedes: current.lease_id };
      const first = await (await requestLease(gateway.token, renewal)).json<LeaseBody>();

      const again = await requestLease(gateway.token, renewal);

      expect(again.status).toBe(200);
      expect(await expectContract("POST", "/v1/gateway/leases", again)).toEqual(first);
      expect(await reservations(user.memberId)).toHaveLength(2);
    });

    it("answers 404 for a lease the API never granted to this gateway", async () => {
      const { gateway, peer } = await servedPeer();
      const other = await servedPeer();
      const foreign = await grantedLease(other.gateway.token, other.peer.peer_id);

      const unknown = await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: uuidv7(), supersedes: crypto.randomUUID() });
      const elsewhere = await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: uuidv7(), supersedes: foreign.lease_id });

      expect([unknown.status, elsewhere.status]).toEqual([404, 404]);
      await expectContract("POST", "/v1/gateway/leases", unknown);
    });

    it("answers 400 for a renewal from another session or peer", async () => {
      const { gateway, lineId, peer } = await servedPeer();
      const current = await grantedLease(gateway.token, peer.peer_id);
      const second = await peerOn(lineId);
      await giveCredits(second.user.memberId, 10_000);

      const otherSession = await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: uuidv7(), supersedes: current.lease_id });
      const otherPeer = await requestLease(gateway.token, { peerId: second.peer.peer_id, sessionId: current.sessionId, supersedes: current.lease_id });

      expect([otherSession.status, otherPeer.status]).toEqual([400, 400]);
    });

    it("answers 402 when the renewal finds no credits, and the current lease keeps its reservation", async () => {
      const { gateway, user, peer } = await servedPeer(LEASE_CREDITS);
      const current = await grantedLease(gateway.token, peer.peer_id);

      const response = await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: current.sessionId, supersedes: current.lease_id });

      expect(response.status).toBe(402);
      expect((await reservations(user.memberId)).map((r) => r.id)).toEqual([current.lease_id]);
    });
  });
});

describe("POST /v1/gateway/leases/{lease_id}/return", () => {
  it("releases what the lease still holds", async () => {
    const { gateway, user, peer } = await servedPeer();
    const lease = await grantedLease(gateway.token, peer.peer_id);

    const response = await returnLease(gateway.token, lease.lease_id);

    expect(response.status).toBe(200);
    expect(await expectContract("POST", "/v1/gateway/leases/{lease_id}/return", response)).toEqual({ released_credits: LEASE_CREDITS });
    expect((await reservations(user.memberId))[0]?.released_at).not.toBeNull();
    // The credits are available for the next lease again.
    expect((await requestLease(gateway.token, { peerId: peer.peer_id, sessionId: uuidv7() })).status).toBe(201);
  });

  it("releases nothing for a lease returned before, and still answers 200", async () => {
    const { gateway, peer } = await servedPeer();
    const lease = await grantedLease(gateway.token, peer.peer_id);
    await returnLease(gateway.token, lease.lease_id);

    const again = await returnLease(gateway.token, lease.lease_id);

    expect(again.status).toBe(200);
    expect(await again.json()).toEqual({ released_credits: 0 });
  });

  it("releases nothing for an expired lease", async () => {
    const { gateway, peer } = await servedPeer();
    const lease = await grantedLease(gateway.token, peer.peer_id);
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.parse(lease.expires_at) + 1000);

    const response = await returnLease(gateway.token, lease.lease_id);

    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ released_credits: 0 });
  });

  it("answers 404 for an unknown lease and for another gateway's lease", async () => {
    const { gateway } = await servedPeer();
    const other = await servedPeer();
    const foreign = await grantedLease(other.gateway.token, other.peer.peer_id);

    const unknown = await returnLease(gateway.token, crypto.randomUUID());
    const elsewhere = await returnLease(gateway.token, foreign.lease_id);

    expect([unknown.status, elsewhere.status]).toEqual([404, 404]);
    await expectContract("POST", "/v1/gateway/leases/{lease_id}/return", unknown);
  });

  it("answers a retry with the same request_id with the first answer", async () => {
    const { gateway, peer } = await servedPeer();
    const lease = await grantedLease(gateway.token, peer.peer_id);
    const requestId = uuidv7();

    await returnLease(gateway.token, lease.lease_id, requestId);
    const retry = await returnLease(gateway.token, lease.lease_id, requestId);

    expect(await retry.json()).toEqual({ released_credits: LEASE_CREDITS });
  });

  it("answers 400 for a malformed lease id", async () => {
    const { gateway } = await servedPeer();

    const response = await returnLease(gateway.token, "not-a-lease");

    expect(response.status).toBe(400);
  });
});
