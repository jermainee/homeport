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
  return `(${owner} = ${member}
    OR EXISTS (SELECT 1 FROM member_links ml
                WHERE (ml.member_id = ${member} AND ml.linked_member_id = ${owner})
                   OR (ml.member_id = ${owner} AND ml.linked_member_id = ${member}))
    OR EXISTS (SELECT 1 FROM auth_sessions mine JOIN auth_sessions theirs ON theirs.install_id = mine.install_id
                WHERE mine.member_id = ${member} AND theirs.member_id = ${owner} AND mine.install_id IS NOT NULL))`;
}

/**
 * SQL condition: the line `line` has room for the member `member`, who may
 * already use it. Both arguments are SQL expressions.
 */
export function lineHasRoomSql(line: string, member: string): string {
  return `((SELECT COUNT(DISTINCT u.member_id) FROM wireguard_peers u
             WHERE u.line_id = ${line} AND u.deleted_at IS NULL AND u.member_id <> ${member}) < ${MAX_USERS_PER_LINE})`;
}
