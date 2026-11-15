import { Hono } from "hono";
import { requireSession, type SessionVariables } from "../auth/middleware";
import { apiError, hasOnlyFields, invalidRequest, readJsonObject } from "../http";
import { COUNTRY, isCity, timestamp } from "../formats";
import { type IdempotentVariables, verifiedMemberIdempotent } from "../idempotency";
import { requireVerifiedMember } from "../identity/require-verified";
import { servedLineSql } from "../gateway/served";
import { KEY32, isName } from "../keys";
import { lineHasRoomSql, lineUsersSql, ownerIsLinkedSql } from "../lines/rules";
import { MAX_PEERS_PER_MEMBER, MAX_USERS_PER_LINE } from "../pricing";
import { FIRST_PEER_HOST, LAST_PEER_HOST, tunnelIpv4, tunnelIpv6, wireguardConfig } from "./config";
import { CURRENT_SESSION_WINDOW_MS, type Failover, isFailover } from "./failover";

/**
 * WireGuard peers: the devices that use a line (contract operations
 * createWireguardPeer, listWireguardPeers, updateWireguardPeer,
 * deleteWireguardPeer).
 */
export const wireguardApi = new Hono<{ Bindings: Env; Variables: SessionVariables & IdempotentVariables }>();



interface PeerRow {
  id: string;
  name: string;
  line_id: string;
  failover: Failover;
  https_only: number;
  public_key: string;
  address_index: number;
  created_at: number;
  line_country: string | null;
  line_city: string | null;
  line_isp: string | null;
  exit_line_id?: string | null;
  exit_country?: string | null;
  exit_city?: string | null;
  exit_since?: number | null;
}

const PEER_COLUMNS = `p.id, p.name, p.line_id, p.failover, p.https_only, p.public_key, p.address_index, p.created_at`;

/**
 * SELECT of PEER_COLUMNS for the peers `p` that WHERE `where` picks, with
 * the line of each peer's newest session the gateway reported within
 * CURRENT_SESSION_WINDOW_MS. The first placeholder (?1) is the window's start.
 */
function peersWithExitSql(where: string): string {
  return `SELECT ${PEER_COLUMNS}, pl.country AS line_country, pl.city AS line_city, pl.isp AS line_isp,
                 s.line_id AS exit_line_id, x.country AS exit_country, x.city AS exit_city, s.started_at AS exit_since
            FROM wireguard_peers p
            JOIN lines pl ON pl.id = p.line_id
            LEFT JOIN usage_sessions s ON s.session_id = (
                   SELECT o.session_id FROM usage_sessions o
                    WHERE o.peer_id = p.id AND o.ended = 0 AND o.updated_at >= ?1
                    ORDER BY o.started_at DESC LIMIT 1)
            LEFT JOIN lines x ON x.id = s.line_id
           WHERE ${where}`;
}

/** A line a member may pick for a peer, with its gateway. */
interface UsableLine {
  gateway_id: string;
  wg_public_key: string;
  wg_endpoint: string;
  linked: number;
}

/**
 * The line `lineId` if a gateway serves it (sharing on, owner active and
 * verified), with its gateway and whether its owner is the member or linked
 * to them. A line no gateway serves is offered to no one.
 */
function usableLine(db: D1Database, lineId: string, memberId: string): Promise<UsableLine | null> {
  return db
    .prepare(
      `SELECT l.gateway_id, g.wg_public_key, g.wg_endpoint, ${ownerIsLinkedSql("?2", "l.member_id")} AS linked
         FROM lines l
         JOIN gateways g ON g.id = l.gateway_id
         JOIN members owner ON owner.id = l.member_id
        WHERE l.id = ?1 AND ${servedLineSql("l", "owner")}`,
    )
    .bind(lineId, memberId)
    .first<UsableLine>();
}

function peerBody(peer: PeerRow) {
  return {
    peer_id: peer.id,
    name: peer.name,
    line_id: peer.line_id,
    line: { country: peer.line_country, city: peer.line_city, isp: peer.line_isp },
    failover: peer.failover,
    https_only: peer.https_only === 1,
    exit_line:
      peer.exit_line_id && peer.exit_since != null
        ? {
            line_id: peer.exit_line_id,
            country: peer.exit_country ?? null,
            city: peer.exit_city ?? null,
            since: timestamp(peer.exit_since),
          }
        : null,
    public_key: peer.public_key,
    ipv4: tunnelIpv4(peer.address_index),
    ipv6: tunnelIpv6(peer.address_index),
    created_at: new Date(peer.created_at).toISOString(),
  };
}

/** Where a new peer goes: one line, or a line the API picks in a region. */
type PeerTarget = { lineId: string } | { country: string; city: string | null };

/** The target a create request names, or why it is invalid. */
function peerTarget(body: Record<string, unknown>): PeerTarget | string {
  const { line_id: lineId, country, city } = body;
  if (lineId !== undefined) {
    if (country !== undefined || city !== undefined) return "send line_id, or country and city, not both";
    return typeof lineId === "string" ? { lineId } : "line_id must be an id";
  }
  if (country === undefined) return "country or line_id is required";
  if (typeof country !== "string" || !COUNTRY.test(country)) return "country must be an ISO 3166-1 alpha-2 code";
  if (city === undefined) return { country, city: null };
  if (!isCity(city)) return "city must have 1 to 200 characters";
  return { country, city };
}

/**
 * SQL query of the one line a new peer of the member ?2 goes on, as column
 * `id`. For one line, the line ?3 if it has room for the member. For a
 * region, the API picks a line in the country ?7, and in the city ?8 when
 * the target names one: served, online, not the member's own or linked to
 * them, with room for the member, and with the fewest other members on it.
 */
function newPeerLineSql(target: PeerTarget): string {
  if ("lineId" in target) return `SELECT ?3 AS id WHERE ${lineHasRoomSql("?3", "?2")}`;
  return `SELECT id FROM (
             SELECT l.id, ${lineUsersSql("l.id", "?2")} AS others
               FROM lines l JOIN members o ON o.id = l.member_id
              WHERE ${servedLineSql("l", "o")} AND l.online = 1 AND l.country = ?7${target.city === null ? "" : " AND l.city = ?8"}
                AND NOT ${ownerIsLinkedSql("?2", "l.member_id")})
           WHERE others < ${MAX_USERS_PER_LINE}
           ORDER BY others, id
           LIMIT 1`;
}

wireguardApi.post("/peers", verifiedMemberIdempotent, async (c) => {
  const body = c.get("body");
  if (!hasOnlyFields(body, ["request_id", "line_id", "country", "city", "name", "public_key", "https_only"])) {
    return invalidRequest(c, "unknown field");
  }
  const target = peerTarget(body);
  if (typeof target === "string") return invalidRequest(c, target);
  const { name, public_key: publicKey, https_only: httpsOnly = true } = body;
  if (typeof httpsOnly !== "boolean") return invalidRequest(c, "https_only must be true or false");
  if (!isName(name)) return invalidRequest(c, "name must have 1 to 64 characters");
  if (typeof publicKey !== "string" || !KEY32.test(publicKey)) {
    return invalidRequest(c, "public_key must be a 32-byte key in standard base64");
  }

  const db = c.env.DB;
  const memberId = c.get("member").id;
  if ("lineId" in target) {
    const line = await usableLine(db, target.lineId, memberId);
    if (!line) return apiError(c, 404, "not_found");
    // No self-dealing: never the member's own line, or a linked member's.
    if (line.linked) return apiError(c, 403, "line_not_allowed");
  }

  // One statement picks or checks the line, takes the lowest free address
  // and checks the member's peer cap. D1 runs it atomically, so concurrent
  // requests never share an address or overfill a line.
  let created: { id: string } | null;
  try {
    const statement = db.prepare(
      `INSERT INTO wireguard_peers (id, member_id, line_id, name, public_key, address_index, created_at, https_only)
       SELECT ?1, ?2, line.id, ?4, ?5, free.host, ?6, ?9
         FROM (${newPeerLineSql(target)}) AS line,
              (SELECT CASE
                 WHEN NOT EXISTS (SELECT 1 FROM wireguard_peers WHERE deleted_at IS NULL AND address_index = ${FIRST_PEER_HOST})
                   THEN ${FIRST_PEER_HOST}
                 ELSE (SELECT MIN(p.address_index) + 1 FROM wireguard_peers p
                        WHERE p.deleted_at IS NULL
                          AND NOT EXISTS (SELECT 1 FROM wireguard_peers q
                                           WHERE q.deleted_at IS NULL AND q.address_index = p.address_index + 1))
               END AS host) AS free
        WHERE free.host <= ${LAST_PEER_HOST}
          AND (SELECT COUNT(*) FROM wireguard_peers mine
                WHERE mine.member_id = ?2 AND mine.deleted_at IS NULL) < ${MAX_PEERS_PER_MEMBER}
       RETURNING id`,
    );
    // ?3 is the one line asked for; ?7 and ?8 the region; ?9 https_only.
    const region = "lineId" in target ? null : target;
    created = await statement
      .bind(
        crypto.randomUUID(),
        memberId,
        "lineId" in target ? target.lineId : null,
        name,
        publicKey,
        Date.now(),
        region?.country ?? null,
        region?.city ?? null,
        httpsOnly ? 1 : 0,
      )
      .first<{ id: string }>();
  } catch (error) {
    if (error instanceof Error && error.message.includes("wireguard_peers.public_key")) {
      return apiError(c, 409, "public_key_taken");
    }
    throw error;
  }
  if (!created) {
    const counts = await db
      .prepare(
        `SELECT (SELECT COUNT(*) FROM wireguard_peers WHERE member_id = ? AND deleted_at IS NULL) AS mine,
                (SELECT COUNT(*) FROM wireguard_peers WHERE deleted_at IS NULL) AS everyone`,
      )
      .bind(memberId)
      .first<{ mine: number; everyone: number }>();
    if ((counts?.mine ?? 0) >= MAX_PEERS_PER_MEMBER) {
      return apiError(c, 429, "rate_limited", `at most ${MAX_PEERS_PER_MEMBER} peers; delete one first`);
    }
    if ((counts?.everyone ?? 0) > LAST_PEER_HOST - FIRST_PEER_HOST) throw new Error("no free tunnel address");
    if (!("lineId" in target)) return apiError(c, 409, "no_free_line", "no free line there right now");
    return apiError(c, 409, "line_full");
  }

  const [peerResult, gatewayResult] = await db.batch([
    db.prepare(peersWithExitSql("p.id = ?2")).bind(Date.now() - CURRENT_SESSION_WINDOW_MS, created.id),
    db
      .prepare(
        `SELECT g.wg_public_key, g.wg_endpoint FROM wireguard_peers p
           JOIN lines l ON l.id = p.line_id JOIN gateways g ON g.id = l.gateway_id WHERE p.id = ?`,
      )
      .bind(created.id),
  ]);
  const peer = peerResult?.results[0] as PeerRow | undefined;
  const gateway = gatewayResult?.results[0] as { wg_public_key: string; wg_endpoint: string } | undefined;
  if (!peer || !gateway) throw new Error("peer missing after insert");
  return c.json(
    {
      peer: peerBody(peer),
      config_text: wireguardConfig({ host: peer.address_index }, { publicKey: gateway.wg_public_key, endpoint: gateway.wg_endpoint }),
    },
    201,
  );
});

wireguardApi.get("/peers", requireSession, async (c) => {
  const { results } = await c.env.DB.prepare(
    `${peersWithExitSql("p.member_id = ?2 AND p.deleted_at IS NULL")} ORDER BY p.created_at DESC, p.rowid DESC`,
  )
    .bind(Date.now() - CURRENT_SESSION_WINDOW_MS, c.get("member").id)
    .all<PeerRow>();
  return c.json({ peers: results.map(peerBody) });
});

/**
 * Moves a peer to another line, or changes its failover or HTTPS only. The device keeps
 * its .conf file, which names the peer's address and the gateway: the line
 * a peer exits through is chosen on the gateway. A new line gets the
 * checks of POST, and must be on the gateway the .conf names.
 */
wireguardApi.patch("/peers/:id", requireVerifiedMember, async (c) => {
  const body = await readJsonObject(c.req.raw);
  if (!body || Object.keys(body).length === 0 || !hasOnlyFields(body, ["line_id", "failover", "https_only"])) {
    return invalidRequest(c, "send line_id, failover, https_only or more of them");
  }
  const { line_id: lineId, failover, https_only: httpsOnly } = body;
  if (lineId !== undefined && typeof lineId !== "string") return invalidRequest(c, "line_id must be an id");
  if (failover !== undefined && !isFailover(failover)) return invalidRequest(c, "failover must be country, city or off");
  if (httpsOnly !== undefined && typeof httpsOnly !== "boolean") return invalidRequest(c, "https_only must be true or false");

  const db = c.env.DB;
  const memberId = c.get("member").id;
  const peerId = c.req.param("id");
  const current = await db
    .prepare(
      `SELECT p.line_id, l.gateway_id FROM wireguard_peers p JOIN lines l ON l.id = p.line_id
        WHERE p.id = ? AND p.member_id = ? AND p.deleted_at IS NULL`,
    )
    .bind(peerId, memberId)
    .first<{ line_id: string; gateway_id: string }>();
  if (!current) return apiError(c, 404, "not_found");

  const newLine = lineId !== undefined && lineId !== current.line_id ? lineId : null;
  if (newLine !== null) {
    const line = await usableLine(db, newLine, memberId);
    if (!line) return apiError(c, 404, "not_found");
    if (line.linked) return apiError(c, 403, "line_not_allowed");
    if (line.gateway_id !== current.gateway_id) return apiError(c, 409, "line_on_other_gateway");
  }

  // Only what the request names changes, so concurrent changes to the other
  // fields survive. The new line's room is checked in the update itself, so
  // two members moving at once never overfill it.
  const changed = await db
    .prepare(
      `UPDATE wireguard_peers
          SET line_id = COALESCE(?1, line_id), failover = COALESCE(?2, failover), https_only = COALESCE(?5, https_only)
        WHERE id = ?3 AND member_id = ?4 AND deleted_at IS NULL
          AND (?1 IS NULL OR ?1 = line_id OR ${lineHasRoomSql("?1", "?4")})
        RETURNING id`,
    )
    .bind(newLine, failover ?? null, peerId, memberId, httpsOnly === undefined ? null : httpsOnly ? 1 : 0)
    .first();
  if (!changed) {
    if (newLine === null) return apiError(c, 404, "not_found");
    const room = await db.prepare(`SELECT ${lineHasRoomSql("?1", "?2")} AS room`).bind(newLine, memberId).first<{ room: number }>();
    return room?.room ? apiError(c, 404, "not_found") : apiError(c, 409, "line_full");
  }

  const peer = await db
    .prepare(peersWithExitSql("p.id = ?2"))
    .bind(Date.now() - CURRENT_SESSION_WINDOW_MS, peerId)
    .first<PeerRow>();
  if (!peer) throw new Error("peer missing after update");
  return c.json(peerBody(peer));
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
