import { servedLineSql } from "../gateway/served";
import { lineHasRoomSql, ownerIsLinkedSql } from "../lines/rules";
import { MAX_FALLBACK_LINES, MAX_USERS_PER_LINE } from "../pricing";

/**
 * Where a peer's traffic goes while its line is offline (contract schema
 * Failover): another line in the same city, else in the same country
 * ("country", the default), in the same city only ("city"), or nowhere
 * ("off"). The gateway moves the peer back to its own line once that line
 * has been online for FAILBACK_SECONDS.
 */
export const FAILOVER_SETTINGS = ["country", "city", "off"] as const;

export type Failover = (typeof FAILOVER_SETTINGS)[number];

export function isFailover(value: unknown): value is Failover {
  return typeof value === "string" && (FAILOVER_SETTINGS as readonly string[]).includes(value);
}

/**
 * SQL condition: the line `f`, owned by the member `fo`, may carry a
 * session of the peer `p` whose own line is `l`. These are the hard rules,
 * which also hold for a session that already runs on `f`: another line on
 * the same gateway, served, and its owner is not the member or linked to
 * them (no self-dealing).
 */
export const TAKEOVER_LINE_SQL = `f.id <> l.id AND f.gateway_id = l.gateway_id
  AND ${servedLineSql("f", "fo")}
  AND NOT ${ownerIsLinkedSql("p.member_id", "f.member_id")}`;

/**
 * SQL condition: a new session of the peer `p` may start on the line `f`
 * (owner `fo`) while its own line `l` is offline. TAKEOVER_LINE_SQL, and the
 * peer's failover allows `f`'s place, and `f` has room for the member.
 * fallbackLines applies the same rules to a whole gateway at once; the two
 * must agree.
 */
export const FALLBACK_LINE_SQL = `${TAKEOVER_LINE_SQL}
  AND p.failover <> 'off' AND f.country = l.country
  AND (p.failover = 'country' OR f.city = l.city)
  AND ${lineHasRoomSql("f.id", "p.member_id")}`;

/** A served peer, with the place of its own line. */
export interface FailoverPeer {
  peer_id: string;
  member_id: string;
  line_id: string;
  failover: Failover;
  country: string | null;
  city: string | null;
}

/** A served line on the same gateway. */
export interface FailoverLine {
  line_id: string;
  owner_member_id: string;
  country: string | null;
  city: string | null;
  /** 1 while its agent is connected, as the gateway's line events say. */
  online: number;
}

/**
 * How recent the gateway's last report of an open session must be for its
 * line to count as the line the peer exits through now. The gateway reports
 * a session that moves bytes at least every 30 seconds, and ends one after 2
 * idle minutes; an older one was left open by a gateway that stopped.
 */
export const CURRENT_SESSION_WINDOW_MS = 3 * 60 * 1000;

/** What fallbackLines needs to know beyond the peers and lines. */
export interface FailoverFacts {
  /** The members with an active peer on each line (its users). */
  users: Map<string, Set<string>>;
  /** Whether two members are linked (see ownerIsLinkedSql). */
  linked: (memberId: string, otherId: string) => boolean;
  /** The line each peer's current session runs on, as the gateway reported it. */
  current?: Map<string, string>;
}

/**
 * Each peer's fallback lines among `lines` (the served lines of its
 * gateway, never another gateway's: the device's .conf names this one), by
 * the rules of FALLBACK_LINE_SQL, best first and at most MAX_FALLBACK_LINES,
 * since the config is a full snapshot: same-city lines, then same-country
 * lines, online ones first within each, so the few listed ones work. Within
 * that the order differs from member to member, so the users of one offline
 * line spread over its fallbacks instead of all landing on one. It is the
 * same for all of a member's devices, so they keep sharing one exit.
 *
 * The line a peer's session runs on now stays in its list while the peer's
 * failover still allows it, even if it has filled up or better lines came
 * online since: the gateway ends a session whose line leaves the list, and
 * that would change the member's IP for nothing.
 *
 * It runs in memory over facts loaded once per gateway, rather than as one
 * SQL condition per peer and line, which grows with peers × lines.
 */
export function fallbackLines(peers: FailoverPeer[], lines: FailoverLine[], facts: FailoverFacts): Map<string, string[]> {
  const byCountry = new Map<string, { line: FailoverLine; hash: number }[]>();
  for (const line of lines) {
    if (line.country === null) continue;
    const list = byCountry.get(line.country) ?? [];
    list.push({ line, hash: fnv1a(line.line_id) });
    byCountry.set(line.country, list);
  }

  const result = new Map<string, string[]>();
  const memo = new Map<string, string[]>();
  for (const peer of peers) {
    const current = facts.current?.get(peer.peer_id);
    const key = `${peer.member_id}\n${peer.line_id}\n${peer.failover}\n${current ?? ""}`;
    let chosen = memo.get(key);
    if (!chosen) {
      chosen = choose(peer, peer.country === null ? [] : (byCountry.get(peer.country) ?? []), facts, current);
      memo.set(key, chosen);
    }
    result.set(peer.peer_id, chosen);
  }
  return result;
}

interface Ranked {
  lineId: string;
  sameCity: boolean;
  online: boolean;
  spread: number;
}

function choose(
  peer: FailoverPeer,
  candidates: { line: FailoverLine; hash: number }[],
  facts: FailoverFacts,
  current: string | undefined,
): string[] {
  if (peer.failover === "off") return [];
  const memberHash = fnv1a(peer.member_id);
  const best: Ranked[] = [];
  let kept = false;
  for (const { line, hash } of candidates) {
    const sameCity = peer.city !== null && line.city === peer.city;
    if (line.line_id === peer.line_id || (peer.failover === "city" && !sameCity)) continue;
    if (line.owner_member_id === peer.member_id || facts.linked(peer.member_id, line.owner_member_id)) continue;
    const isCurrent = line.line_id === current;
    if (!isCurrent && !hasRoom(facts.users.get(line.line_id), peer.member_id)) continue;
    kept ||= isCurrent;
    insertRanked(best, { lineId: line.line_id, sameCity, online: line.online === 1, spread: mix(memberHash, hash) });
  }
  const chosen = best.map((ranked) => ranked.lineId);
  if (kept && current && !chosen.includes(current)) chosen.splice(MAX_FALLBACK_LINES - 1, 1, current);
  return chosen;
}

/** Keeps `best` sorted and at most MAX_FALLBACK_LINES long. */
function insertRanked(best: Ranked[], next: Ranked): void {
  let at = best.length;
  while (at > 0 && before(next, best[at - 1] as Ranked)) at--;
  if (at >= MAX_FALLBACK_LINES) return;
  best.splice(at, 0, next);
  if (best.length > MAX_FALLBACK_LINES) best.pop();
}

function before(a: Ranked, b: Ranked): boolean {
  if (a.sameCity !== b.sameCity) return a.sameCity;
  if (a.online !== b.online) return a.online;
  if (a.spread !== b.spread) return a.spread < b.spread;
  return a.lineId < b.lineId;
}

/** lineHasRoomSql for a line whose users are `users`. */
function hasRoom(users: Set<string> | undefined, memberId: string): boolean {
  if (!users) return true;
  return users.size - (users.has(memberId) ? 1 : 0) < MAX_USERS_PER_LINE;
}

/** 32-bit FNV-1a of a string's UTF-16 code units. */
function fnv1a(text: string): number {
  let hash = 0x811c9dc5;
  for (let i = 0; i < text.length; i++) {
    hash ^= text.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash;
}

/** A well-spread 32-bit hash of two hashes (murmur3's finalizer). */
function mix(a: number, b: number): number {
  let h = (a ^ Math.imul(b, 0x9e3779b1)) >>> 0;
  h = Math.imul(h ^ (h >>> 16), 0x85ebca6b);
  h = Math.imul(h ^ (h >>> 13), 0xc2b2ae35);
  return (h ^ (h >>> 16)) >>> 0;
}
