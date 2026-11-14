import { sha256Hex } from "../crypto";
import { HOSTNAME } from "../formats";
import { httpOrigin } from "../http";
import { FAILBACK_SECONDS } from "../pricing";
import { tunnelIpv4, tunnelIpv6 } from "../wireguard/config";
import { FALLBACK_LINE_SQL, type FallbackCandidate, fallbackLines } from "../wireguard/failover";
import { PEER_JOINS, SERVED_LINE_SQL, SERVED_PEER_SQL } from "./served";

/** The contract's GatewayConfig. */
export interface GatewayConfig {
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

/**
 * Everything a gateway serves: its shared lines with their agents' SPKI
 * pins (hex, as the devices table stores them), the peers it may accept
 * on them with the lines that take over while a peer's line is offline,
 * and the hosts a peer without credits can still reach. The etag is a hash
 * of the rest, so it changes exactly when the config does.
 */
export async function gatewayConfig(db: D1Database, env: Env, gatewayId: string): Promise<GatewayConfig> {
  const [peers, lines, fallbacks] = await db.batch([
    db
      .prepare(
        `SELECT p.id AS peer_id, p.member_id, p.public_key, p.address_index, p.line_id
           FROM ${PEER_JOINS}
          WHERE l.gateway_id = ? AND ${SERVED_PEER_SQL}
          ORDER BY p.id`,
      )
      .bind(gatewayId),
    db
      .prepare(
        `SELECT l.id AS line_id, l.member_id AS owner_member_id, d.spki_sha256 AS device_spki_sha256
           FROM lines l JOIN members o ON o.id = l.member_id JOIN devices d ON d.id = l.device_id
          WHERE l.gateway_id = ? AND ${SERVED_LINE_SQL}
          ORDER BY l.id`,
      )
      .bind(gatewayId),
    db
      .prepare(
        `SELECT p.id AS peer_id, p.member_id, f.id AS line_id, (f.city IS NOT NULL AND f.city = l.city) AS same_city
           FROM ${PEER_JOINS} JOIN lines f JOIN members fo ON fo.id = f.member_id
          WHERE l.gateway_id = ? AND ${SERVED_PEER_SQL} AND ${FALLBACK_LINE_SQL}`,
      )
      .bind(gatewayId),
  ]);
  const fallbackIds = fallbackLines((fallbacks?.results ?? []) as FallbackCandidate[]);
  const content = {
    topup_hostnames: topupHostnames(env),
    failback_seconds: FAILBACK_SECONDS,
    peers: ((peers?.results ?? []) as PeerRow[]).map((peer) => ({
      peer_id: peer.peer_id,
      member_id: peer.member_id,
      public_key: peer.public_key,
      ipv4: tunnelIpv4(peer.address_index),
      ipv6: tunnelIpv6(peer.address_index),
      line_id: peer.line_id,
      fallback_line_ids: fallbackIds.get(peer.peer_id) ?? [],
    })),
    lines: (lines?.results ?? []) as GatewayConfig["lines"],
  };
  const etag = `"cfg-${(await sha256Hex(JSON.stringify(content))).slice(0, 32)}"`;
  return { etag, ...content };
}

interface PeerRow {
  peer_id: string;
  member_id: string;
  public_key: string;
  address_index: number;
  line_id: string;
}

/**
 * Hosts a peer without credits can still reach: the API itself (from
 * PUBLIC_BASE_URL), so the app can start a top-up, then TOPUP_HOSTNAMES
 * (comma-separated), such as Stripe Checkout's hosts.
 */
export function topupHostnames(env: Env): string[] {
  const api = httpOrigin(env.PUBLIC_BASE_URL)?.hostname;
  const configured = env.TOPUP_HOSTNAMES.split(",").map((host) => host.trim().toLowerCase());
  const hosts = [...new Set([api, ...configured])].filter((host): host is string => !!host && HOSTNAME.test(host));
  if (hosts.length === 0) throw new Error("no top-up hostnames: set PUBLIC_BASE_URL or TOPUP_HOSTNAMES");
  return hosts;
}

/**
 * True when an If-None-Match header names `etag`. Tags compare weakly
 * (RFC 9110), since Cloudflare may weaken the ETag header when it compresses
 * the response.
 */
export function matchesEtag(ifNoneMatch: string | undefined, etag: string): boolean {
  if (!ifNoneMatch) return false;
  if (ifNoneMatch.trim() === "*") return true;
  return ifNoneMatch.split(",").some((tag) => tag.trim().replace(/^W\//, "") === etag);
}
