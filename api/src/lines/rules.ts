import { MAX_USERS_PER_LINE } from "../pricing";

/**
 * SQL condition: the line owner `owner` is the member `member` or linked to
 * them, so the member may not use the owner's lines. Both arguments are SQL
 * expressions, such as a placeholder or a column.
 *
 * Linked means a member_links row in either direction (household, device,
 * card, Connect account), or a session of each from the same app install.
 */
export function ownerIsLinkedSql(member: string, owner: string): string {
  // A placeholder is one member for the whole statement: SQLite builds their
  // linked set once, which keeps a scan over many lines cheap. A column is a
  // different member on each row: probing per row is about 4 times cheaper
  // there than building a set per row. Both forms say the same.
  if (/^\?\d+$/.test(member)) return `(${owner} IN (${linkedMembersSql(member)}))`;
  return `(${owner} = ${member}
    OR EXISTS (SELECT 1 FROM member_links ml
                WHERE (ml.member_id = ${member} AND ml.linked_member_id = ${owner})
                   OR (ml.member_id = ${owner} AND ml.linked_member_id = ${member}))
    OR EXISTS (SELECT 1 FROM auth_sessions mine JOIN auth_sessions theirs ON theirs.install_id = mine.install_id
                WHERE mine.member_id = ${member} AND theirs.member_id = ${owner} AND mine.install_id IS NOT NULL))`;
}

/**
 * SQL query: the member `member` and every member linked to them, as
 * ownerIsLinkedSql defines it. No row is NULL, so NOT IN works on it.
 */
function linkedMembersSql(member: string): string {
  return `SELECT ${member}
    UNION SELECT ml.linked_member_id FROM member_links ml WHERE ml.member_id = ${member}
    UNION SELECT ml.member_id FROM member_links ml WHERE ml.linked_member_id = ${member}
    UNION SELECT theirs.member_id FROM auth_sessions mine JOIN auth_sessions theirs ON theirs.install_id = mine.install_id
           WHERE mine.member_id = ${member} AND mine.install_id IS NOT NULL`;
}

/**
 * SQL query of the pairs (member_id, linked_id) that ownerIsLinkedSql calls
 * linked, for each member the SQL query `members` returns: member_links rows
 * in either direction, and members with sessions from the same app install.
 * The two definitions must agree; this one serves many members at once.
 */
export function linkedPairsSql(members: string): string {
  return `SELECT ml.member_id, ml.linked_member_id AS linked_id FROM member_links ml WHERE ml.member_id IN (${members})
    UNION SELECT ml.linked_member_id, ml.member_id FROM member_links ml WHERE ml.linked_member_id IN (${members})
    UNION SELECT mine.member_id, theirs.member_id FROM auth_sessions mine
            JOIN auth_sessions theirs ON theirs.install_id = mine.install_id
           WHERE mine.install_id IS NOT NULL AND mine.member_id IN (${members}) AND theirs.member_id <> mine.member_id`;
}

/**
 * SQL condition: the line `line` has room for the member `member`, who may
 * already use it. Without `member`, room for a member who doesn't use it
 * yet. The arguments are SQL expressions.
 */
export function lineHasRoomSql(line: string, member?: string): string {
  return `(${lineUsersSql(line, member)} < ${MAX_USERS_PER_LINE})`;
}

/**
 * SQL expression: how many members use the line `line` (have an active
 * peer on it), not counting the member `member`. The arguments are SQL
 * expressions.
 */
export function lineUsersSql(line: string, member?: string): string {
  const others = member === undefined ? "" : ` AND u.member_id <> ${member}`;
  return `(SELECT COUNT(DISTINCT u.member_id) FROM wireguard_peers u
             WHERE u.line_id = ${line} AND u.deleted_at IS NULL${others})`;
}
