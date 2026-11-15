-- Migration number: 0010 	 2026-11-15T00:00:00.000Z
-- Line canaries. A gateway that catches a line changing a response (a
-- modified agent, or an ISP's transparent proxy) reports the line
-- tampered, and the line is paused: no gateway serves it and nobody can
-- pick it until someone reviews it. Code: src/gateway/line-events.ts and
-- src/gateway/served.ts.
ALTER TABLE lines ADD COLUMN paused_at INTEGER;
-- tampered_http | tampered_dns
ALTER TABLE lines ADD COLUMN paused_reason TEXT;

-- line_events gains the tampered type and the check that caught the line.
-- SQLite cannot change a CHECK constraint, so the table is rebuilt.
CREATE TABLE line_events_new (
  event_id TEXT PRIMARY KEY,
  gateway_id TEXT NOT NULL,
  line_id TEXT NOT NULL REFERENCES lines (id),
  -- online | offline | tampered
  type TEXT NOT NULL CHECK (type IN ('online', 'offline', 'tampered')),
  -- http | dns for a tampered event, NULL for the others
  tamper_check TEXT CHECK (
    (type = 'tampered' AND tamper_check IN ('http', 'dns')) OR (type <> 'tampered' AND tamper_check IS NULL)
  ),
  agent_version TEXT NOT NULL,
  at INTEGER NOT NULL,
  received_at INTEGER NOT NULL
);
INSERT INTO line_events_new (event_id, gateway_id, line_id, type, agent_version, at, received_at)
  SELECT event_id, gateway_id, line_id, type, agent_version, at, received_at FROM line_events;
DROP TABLE line_events;
ALTER TABLE line_events_new RENAME TO line_events;
CREATE INDEX line_events_received ON line_events (received_at);
