-- Migration number: 0009 	 2026-11-14T00:00:00.000Z
-- Members pick a line by country and city (GET /v1/lines/regions). Code:
-- src/lines/regions.ts.

-- Finds a country's or a city's shared lines without reading every line.
CREATE INDEX lines_region ON lines (sharing_enabled, country, city, online);

-- Free lines per region: shared, online and with room for one more member.
-- Counting them takes about a second at 300,000 lines, so the cron trigger
-- recounts them every minute instead of each request. A request corrects
-- the counts for its member.
CREATE TABLE line_region_counts (
  country TEXT NOT NULL,
  -- '' when the city is not known.
  city TEXT NOT NULL,
  lines INTEGER NOT NULL,
  PRIMARY KEY (country, city)
) WITHOUT ROWID;

-- When line_region_counts was last recounted. At most one row.
CREATE TABLE line_region_recounts (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  -- Unix epoch milliseconds.
  counted_at INTEGER NOT NULL
);
