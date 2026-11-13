import { ownerIsLinkedSql } from "../lines/rules";

/**
 * SQL condition, with `l` a line and `o` its owner (members): the line is
 * shared and its owner may share. Such lines are in a gateway's config.
 */
export const SERVED_LINE_SQL = `l.sharing_enabled = 1 AND o.status = 'active' AND o.identity_status = 'verified'`;

/**
 * SQL condition, with `p` a WireGuard peer, `m` its member, `l` its line and
 * `o` the line's owner: the gateway serves the peer. The peer is not
 * deleted, its line is served, its member is active and verified, and the
 * member is not the owner or linked to them (no self-dealing).
 */
export const SERVED_PEER_SQL = `p.deleted_at IS NULL AND ${SERVED_LINE_SQL}
  AND m.status = 'active' AND m.identity_status = 'verified'
  AND NOT ${ownerIsLinkedSql("p.member_id", "l.member_id")}`;

/** FROM clause joining `p`, `m`, `l` and `o` for SERVED_PEER_SQL. */
export const PEER_JOINS = `wireguard_peers p
  JOIN members m ON m.id = p.member_id
  JOIN lines l ON l.id = p.line_id
  JOIN members o ON o.id = l.member_id`;
