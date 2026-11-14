import { servedLineSql } from "../gateway/served";
import { lineHasRoomSql, ownerIsLinkedSql } from "../lines/rules";
import { MAX_FALLBACK_LINES } from "../pricing";

/**
 * Where a peer's traffic goes while its line is offline (contract schema
 * Failover): another line in the same city, else in the same country
 * ("country", the default), in the same city only ("city"), or nowhere
 * ("off"). The gateway moves the peer back to its own line once that line
 * has been online for 5 minutes.
 */
export const FAILOVER_SETTINGS = ["country", "city", "off"] as const;

export type Failover = (typeof FAILOVER_SETTINGS)[number];

export function isFailover(value: unknown): value is Failover {
  return typeof value === "string" && (FAILOVER_SETTINGS as readonly string[]).includes(value);
}

/**
 * SQL condition: the line `f`, owned by the member `fo`, may take over the
 * line `l` of the peer `p`. It is another line on the same gateway, in the
 * same city or (for failover "country") the same country, it is served, it
 * has room for the peer's member, and its owner is not the member or linked
 * to them (no self-dealing).
 */
export const FALLBACK_LINE_SQL = `p.failover <> 'off'
  AND f.id <> l.id AND f.gateway_id = l.gateway_id AND f.country = l.country
  AND (p.failover = 'country' OR f.city = l.city)
  AND ${servedLineSql("f", "fo")}
  AND NOT ${ownerIsLinkedSql("p.member_id", "f.member_id")}
  AND ${lineHasRoomSql("f.id", "p.member_id")}`;

/** A line that may take over a peer's line, as FALLBACK_LINE_SQL finds it. */
export interface FallbackCandidate {
  peer_id: string;
  member_id: string;
  line_id: string;
  same_city: number;
}

/**
 * Each peer's fallback lines, best first and at most MAX_FALLBACK_LINES:
 * same-city lines, then same-country lines. Within each group the order
 * differs from member to member, so the users of one offline line spread
 * over its fallbacks instead of all landing on one. It is the same for all
 * of a member's devices, so they keep sharing one exit, and it stays the
 * same from one config to the next.
 */
export function fallbackLines(candidates: FallbackCandidate[]): Map<string, string[]> {
  const byPeer = new Map<string, FallbackCandidate[]>();
  for (const candidate of candidates) {
    const list = byPeer.get(candidate.peer_id) ?? [];
    list.push(candidate);
    byPeer.set(candidate.peer_id, list);
  }
  const result = new Map<string, string[]>();
  for (const [peerId, list] of byPeer) {
    const ranked = list
      .map((candidate) => ({ candidate, spread: fnv1a(`${candidate.member_id}/${candidate.line_id}`) }))
      .sort((a, b) => b.candidate.same_city - a.candidate.same_city || a.spread - b.spread || compare(a.candidate.line_id, b.candidate.line_id));
    result.set(
      peerId,
      ranked.slice(0, MAX_FALLBACK_LINES).map(({ candidate }) => candidate.line_id),
    );
  }
  return result;
}

function compare(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** 32-bit FNV-1a of a string's UTF-16 code units: cheap, stable, well spread. */
function fnv1a(text: string): number {
  let hash = 0x811c9dc5;
  for (let i = 0; i < text.length; i++) {
    hash ^= text.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash;
}
