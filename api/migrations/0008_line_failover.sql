-- Migration number: 0008 	 2026-11-14T00:00:00.000Z
-- Failover: where a WireGuard peer's traffic goes while its line is offline.
-- country (another line in the same city, else in the same country), city
-- (the same city only) or off. Code: src/wireguard/failover.ts.
ALTER TABLE wireguard_peers ADD COLUMN failover TEXT NOT NULL DEFAULT 'country'
  CHECK (failover IN ('country', 'city', 'off'));

-- Fallback lines are looked up by gateway and place.
CREATE INDEX lines_gateway_place ON lines (gateway_id, country, city);

-- A peer's open sessions, newest first: the line it exits through now.
CREATE INDEX usage_sessions_peer_open ON usage_sessions (peer_id, started_at) WHERE ended = 0;
