import { Hono } from "hono";
import { every } from "hono/combine";
import { requireSession, type SessionVariables } from "../auth/middleware";
import { apiError, hasOnlyFields, invalidRequest } from "../http";
import { type IdempotentVariables, idempotent } from "../idempotency";
import { requireVerifiedIdentity } from "../identity/require-verified";
import { KEY32, isName } from "../keys";
import { lineHasRoomSql, ownerIsLinkedSql } from "../lines/rules";
import { MAX_PEERS_PER_MEMBER } from "../pricing";
import { FIRST_PEER_HOST, LAST_PEER_HOST, tunnelIpv4, tunnelIpv6, wireguardConfig } from "./config";

/**
 * WireGuard peers: the devices that use a line (contract operations
 * createWireguardPeer, listWireguardPeers, deleteWireguardPeer).
 */
export const wireguardApi = new Hono<{ Bindings: Env; Variables: SessionVariables & IdempotentVariables }>();

// The identity check runs before idempotent, so a 403 is not stored for the request_id.
const verifiedRequest = every(requireSession, requireVerifiedIdentity, idempotent);

interface PeerRow {
  id: string;
  name: string;
  line_id: string;
  public_key: string;
  address_index: number;
  created_at: number;
}

function peerBody(peer: PeerRow) {
  return {
    peer_id: peer.id,
    name: peer.name,
    line_id: peer.line_id,
    public_key: peer.public_key,
    ipv4: tunnelIpv4(peer.address_index),
    ipv6: tunnelIpv6(peer.address_index),
    created_at: new Date(peer.created_at).toISOString(),
  };
}

wireguardApi.post("/peers", verifiedRequest, async (c) => {
  const body = c.get("body");
  if (!hasOnlyFields(body, ["request_id", "line_id", "name", "public_key"])) return invalidRequest(c, "unknown field");
  const { line_id: lineId, name, public_key: publicKey } = body;
  if (typeof lineId !== "string") return invalidRequest(c, "line_id is required");
  if (!isName(name)) return invalidRequest(c, "name must have 1 to 64 characters");
  if (typeof publicKey !== "string" || !KEY32.test(publicKey)) {
    return invalidRequest(c, "public_key must be a 32-byte key in standard base64");
  }

  const db = c.env.DB;
  const memberId = c.get("member").id;
  const line = await db
    .prepare(
      `SELECT l.member_id, g.wg_public_key, g.wg_endpoint, ${ownerIsLinkedSql("?2", "l.member_id")} AS linked
         FROM lines l
         JOIN gateways g ON g.id = l.gateway_id
         JOIN members owner ON owner.id = l.member_id AND owner.status = 'active'
        WHERE l.id = ?1 AND l.sharing_enabled = 1`,
    )
    .bind(lineId, memberId)
    .first<{ member_id: string; wg_public_key: string; wg_endpoint: string; linked: number }>();
  // A line that is not sharing, or whose owner is suspended, is offered to no one.
  if (!line) return apiError(c, 404, "not_found");
  // No self-dealing: never the member's own line, or a linked member's.
  if (line.linked) return apiError(c, 403, "line_not_allowed");

  // One statement takes the lowest free address and checks the line's room
  // and the member's peer cap. D1 runs it atomically, so concurrent requests
  // never share an address or overfill a line.
  let peer: PeerRow | null;
  try {
    peer = await db
      .prepare(
        `INSERT INTO wireguard_peers (id, member_id, line_id, name, public_key, address_index, created_at)
         SELECT ?1, ?2, ?3, ?4, ?5, free.host, ?6
           FROM (SELECT CASE
                   WHEN NOT EXISTS (SELECT 1 FROM wireguard_peers WHERE deleted_at IS NULL AND address_index = ${FIRST_PEER_HOST})
                     THEN ${FIRST_PEER_HOST}
                   ELSE (SELECT MIN(p.address_index) + 1 FROM wireguard_peers p
                          WHERE p.deleted_at IS NULL
                            AND NOT EXISTS (SELECT 1 FROM wireguard_peers q
                                             WHERE q.deleted_at IS NULL AND q.address_index = p.address_index + 1))
                 END AS host) AS free
          WHERE free.host <= ${LAST_PEER_HOST} AND ${lineHasRoomSql("?3", "?2")}
            AND (SELECT COUNT(*) FROM wireguard_peers mine
                  WHERE mine.member_id = ?2 AND mine.deleted_at IS NULL) < ${MAX_PEERS_PER_MEMBER}
         RETURNING id, name, line_id, public_key, address_index, created_at`,
      )
      .bind(crypto.randomUUID(), memberId, lineId, name, publicKey, Date.now())
      .first<PeerRow>();
  } catch (error) {
    if (error instanceof Error && error.message.includes("wireguard_peers.public_key")) {
      return apiError(c, 409, "public_key_taken");
    }
    throw error;
  }
  if (!peer) {
    const limits = await db
      .prepare(
        `SELECT ${lineHasRoomSql("?1", "?2")} AS room,
                (SELECT COUNT(*) FROM wireguard_peers WHERE member_id = ?2 AND deleted_at IS NULL) AS peers`,
      )
      .bind(lineId, memberId)
      .first<{ room: number; peers: number }>();
    if (!limits?.room) return apiError(c, 409, "line_full");
    if (limits.peers >= MAX_PEERS_PER_MEMBER) {
      return apiError(c, 429, "rate_limited", `at most ${MAX_PEERS_PER_MEMBER} peers; delete one first`);
    }
    throw new Error("no free tunnel address");
  }

  return c.json(
    {
      peer: peerBody(peer),
      config_text: wireguardConfig(
        { host: peer.address_index },
        { publicKey: line.wg_public_key, endpoint: line.wg_endpoint },
      ),
    },
    201,
  );
});

wireguardApi.get("/peers", requireSession, async (c) => {
  const { results } = await c.env.DB.prepare(
    `SELECT id, name, line_id, public_key, address_index, created_at FROM wireguard_peers
      WHERE member_id = ? AND deleted_at IS NULL ORDER BY created_at DESC, rowid DESC`,
  )
    .bind(c.get("member").id)
    .all<PeerRow>();
  return c.json({ peers: results.map(peerBody) });
});

/**
 * Deleting keeps the row (deleted_at), so usage the gateway reports late
 * still finds its member; the key and the address are free again.
 */
wireguardApi.delete("/peers/:id", requireSession, async (c) => {
  const deleted = await c.env.DB.prepare(
    `UPDATE wireguard_peers SET deleted_at = ? WHERE id = ? AND member_id = ? AND deleted_at IS NULL`,
  )
    .bind(Date.now(), c.req.param("id"), c.get("member").id)
    .run();
  if (deleted.meta.changes !== 1) return apiError(c, 404, "not_found");
  return c.body(null, 204);
});
