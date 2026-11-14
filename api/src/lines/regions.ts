import { SERVED_LINE_SQL } from "../gateway/served";
import { lineHasRoomSql, ownerIsLinkedSql } from "./rules";

/** The contract's CityRegion. */
export interface CityRegion {
  city: string;
  lines: number;
}

/** The contract's CountryRegion. */
export interface CountryRegion {
  country: string;
  lines: number;
  cities?: CityRegion[];
}

/** The cron trigger (wrangler.jsonc) that recounts the free lines in each region. */
export const RECOUNT_CRON = "* * * * *";

/**
 * A request recounts the regions itself once the last recount is this old,
 * for example in local development, where no cron trigger runs.
 */
export const RECOUNT_STALE_MS = 2 * 60 * 1000;

/**
 * FROM clause and SQL condition: shared lines in a known country that are
 * online and whose owner may share. `l` is the line, `o` its owner.
 */
const ONLINE_LINES = `lines l JOIN members o ON o.id = l.member_id
  WHERE ${SERVED_LINE_SQL} AND l.online = 1 AND l.country IS NOT NULL`;

/**
 * Recounts line_region_counts: per country and city, the online lines with
 * room for one more member. The cron trigger runs it every minute; it reads
 * every online line, which takes about a second at 300,000 lines.
 */
export async function recountLineRegions(db: D1Database, now: number): Promise<void> {
  await db.batch([
    db.prepare(`DELETE FROM line_region_counts`),
    db.prepare(
      `INSERT INTO line_region_counts (country, city, lines)
       SELECT l.country, COALESCE(l.city, ''), COUNT(*) FROM ${ONLINE_LINES} AND ${lineHasRoomSql("l.id")}
        GROUP BY l.country, l.city`,
    ),
    db
      .prepare(
        `INSERT INTO line_region_recounts (id, counted_at) VALUES (1, ?)
         ON CONFLICT (id) DO UPDATE SET counted_at = excluded.counted_at`,
      )
      .bind(now),
  ]);
}

/**
 * The countries with free lines for `memberId`, and how many each has
 * (contract operation listLineRegions). With `country`, that country alone
 * and its cities, most lines first.
 *
 * The counts come from the last recount, at most about a minute old, and
 * are corrected for the member at once: lines of the member and of members
 * linked to them are taken off, and full lines the member already uses are
 * added back, since a line has room for those who use it.
 */
export async function lineRegions(db: D1Database, memberId: string, country?: string): Promise<CountryRegion[]> {
  const now = Date.now();
  const recount = await db.prepare(`SELECT counted_at FROM line_region_recounts`).first<{ counted_at: number }>();
  if (!recount || now - recount.counted_at > RECOUNT_STALE_MS) await recountLineRegions(db, now);

  if (country === undefined) {
    const { results } = await db
      .prepare(`SELECT country, SUM(lines) AS lines FROM (${memberRegionsSql(false)}) GROUP BY country ORDER BY country`)
      .bind(memberId)
      .all<CountryRegion>();
    return results;
  }

  const { results } = await db
    .prepare(`${memberRegionsSql(true)} ORDER BY lines DESC, city`)
    .bind(memberId, country)
    .all<{ city: string; lines: number }>();
  if (results.length === 0) return [];
  return [
    {
      country,
      lines: results.reduce((sum, row) => sum + row.lines, 0),
      cities: results.filter((row) => row.city !== "").map(({ city, lines }) => ({ city, lines })),
    },
  ];
}

/**
 * SQL query: the free lines per country and city for the member ?1, as
 * columns country, city ('' when not known) and lines. With `inCountry`,
 * only the regions in the country ?2.
 */
function memberRegionsSql(inCountry: boolean): string {
  const where = (column: string) => (inCountry ? ` AND ${column} = ?2` : "");
  return `SELECT country, city, SUM(n) AS lines FROM (
       SELECT country, city, lines AS n FROM line_region_counts WHERE 1${where("country")}
       UNION ALL
       SELECT l.country, COALESCE(l.city, ''), -1 FROM ${ONLINE_LINES}${where("l.country")}
          AND ${ownerIsLinkedSql("?1", "l.member_id")} AND ${lineHasRoomSql("l.id")}
       UNION ALL
       SELECT l.country, COALESCE(l.city, ''), 1 FROM ${ONLINE_LINES}${where("l.country")}
          AND l.id IN (SELECT line_id FROM wireguard_peers WHERE member_id = ?1 AND deleted_at IS NULL)
          AND NOT ${ownerIsLinkedSql("?1", "l.member_id")}
          AND ${lineHasRoomSql("l.id", "?1")} AND NOT ${lineHasRoomSql("l.id")}
     ) GROUP BY country, city HAVING SUM(n) > 0`;
}
