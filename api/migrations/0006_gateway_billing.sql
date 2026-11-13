-- Gateway endpoints: gateway credentials, quota leases, usage events and the
-- session log, line events. Code: src/gateway/, src/prune.ts.
-- Timestamps are Unix epoch milliseconds (INTEGER).

-- A gateway's token is hpgw_<gateway_id>.<secret>. Only the secret's hash
-- is stored. The credential exists before the gateway first announces
-- itself (PUT /v1/gateway/self writes gateways), so it has its own table.
CREATE TABLE gateway_credentials (
  gateway_id TEXT PRIMARY KEY,
  -- Lowercase hex SHA-256 of the secret.
  secret_sha256 TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
);

-- A quota lease: bytes a peer's session may use, backed by the credit
-- reservation with the same id (credit_reservations.id).
CREATE TABLE leases (
  id TEXT PRIMARY KEY,
  gateway_id TEXT NOT NULL REFERENCES gateways (id),
  peer_id TEXT NOT NULL REFERENCES wireguard_peers (id),
  -- The peer's member, who pays.
  member_id TEXT NOT NULL REFERENCES members (id),
  line_id TEXT NOT NULL REFERENCES lines (id),
  -- The gateway's session (UUIDv7).
  session_id TEXT NOT NULL,
  -- Credits reserved, and what they buy at the use rate (pricing.ts).
  credits INTEGER NOT NULL CHECK (credits > 0),
  granted_bytes INTEGER NOT NULL CHECK (granted_bytes > 0),
  renew_at_bytes INTEGER NOT NULL CHECK (renew_at_bytes > 0 AND renew_at_bytes < granted_bytes),
  grace_seconds INTEGER NOT NULL CHECK (grace_seconds >= 0),
  expires_at INTEGER NOT NULL,
  -- The session's previous lease, for a renewal.
  supersedes_lease_id TEXT REFERENCES leases (id),
  returned_at INTEGER,
  created_at INTEGER NOT NULL
);

-- A lease is superseded at most once: a second renewal aborts its batch.
CREATE UNIQUE INDEX leases_supersedes ON leases (supersedes_lease_id) WHERE supersedes_lease_id IS NOT NULL;
CREATE INDEX leases_session ON leases (session_id);

-- The session log: who used which line, when, and how many bytes. Only
-- session metadata; nothing about destinations or content.
CREATE TABLE usage_sessions (
  -- The gateway's session (UUIDv7).
  session_id TEXT PRIMARY KEY,
  gateway_id TEXT NOT NULL REFERENCES gateways (id),
  peer_id TEXT NOT NULL REFERENCES wireguard_peers (id),
  member_id TEXT NOT NULL REFERENCES members (id),
  line_id TEXT NOT NULL REFERENCES lines (id),
  owner_member_id TEXT NOT NULL REFERENCES members (id),
  -- The line's public IP when the session started, as the gateway saw it.
  exit_ip TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  -- period_end of the newest applied event; the session's end once ended.
  last_period_end INTEGER NOT NULL,
  -- seq the next event must carry.
  next_seq INTEGER NOT NULL CHECK (next_seq > 0),
  bytes_up INTEGER NOT NULL CHECK (bytes_up >= 0),
  bytes_down INTEGER NOT NULL CHECK (bytes_down >= 0),
  -- 1 once the final event arrived.
  ended INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);

CREATE INDEX usage_sessions_member ON usage_sessions (member_id, started_at);
CREATE INDEX usage_sessions_line ON usage_sessions (line_id, started_at);

-- Inserting into this view adds one usage event to its session's log, and
-- aborts the whole batch (the event's charge included) unless the session
-- is open and expects exactly that seq. So the log never misses a charged
-- event, even when two requests race. Nothing is stored in the view.
CREATE VIEW usage_session_steps AS
  SELECT NULL AS session_id, NULL AS seq, NULL AS period_end, NULL AS bytes_up, NULL AS bytes_down,
         NULL AS final, NULL AS updated_at
   WHERE 0;

CREATE TRIGGER usage_session_step INSTEAD OF INSERT ON usage_session_steps
BEGIN
  SELECT RAISE(ABORT, 'usage event out of step with its session')
   WHERE NOT EXISTS (SELECT 1 FROM usage_sessions
                      WHERE session_id = NEW.session_id AND next_seq = NEW.seq AND ended = 0);
  UPDATE usage_sessions
     SET next_seq = next_seq + 1, last_period_end = NEW.period_end, bytes_up = bytes_up + NEW.bytes_up,
         bytes_down = bytes_down + NEW.bytes_down, ended = NEW.final, updated_at = NEW.updated_at
   WHERE session_id = NEW.session_id;
END;

-- Raw usage events, kept 7 days after they arrive so a retry is answered
-- duplicate (the contract keeps event ids 7 days). The cron deletes them.
CREATE TABLE usage_events (
  event_id TEXT PRIMARY KEY,
  gateway_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  peer_id TEXT NOT NULL,
  line_id TEXT NOT NULL,
  lease_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  bytes_up INTEGER NOT NULL,
  bytes_down INTEGER NOT NULL,
  period_start INTEGER NOT NULL,
  period_end INTEGER NOT NULL,
  final INTEGER NOT NULL,
  received_at INTEGER NOT NULL
);

CREATE INDEX usage_events_received ON usage_events (received_at);
CREATE INDEX usage_charges_created ON usage_charges (created_at);

-- Raw line events, kept 7 days like usage events.
CREATE TABLE line_events (
  event_id TEXT PRIMARY KEY,
  gateway_id TEXT NOT NULL,
  line_id TEXT NOT NULL REFERENCES lines (id),
  -- online | offline
  type TEXT NOT NULL CHECK (type IN ('online', 'offline')),
  agent_version TEXT NOT NULL,
  at INTEGER NOT NULL,
  received_at INTEGER NOT NULL
);

CREATE INDEX line_events_received ON line_events (received_at);

-- When the newest applied line event happened, so an older one that
-- arrives late changes nothing.
ALTER TABLE lines ADD COLUMN online_changed_at INTEGER;
