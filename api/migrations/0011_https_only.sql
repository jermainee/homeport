-- Migration number: 0011 	 2026-11-15T00:00:00.000Z
-- HTTPS only, per WireGuard peer: the gateway admits only TLS on TCP from
-- the device, so the line's owner and their ISP can neither read nor change
-- its traffic. On by default, existing devices included (decided
-- 2026-11-15); a member can turn it off per device.
ALTER TABLE wireguard_peers ADD COLUMN https_only INTEGER NOT NULL DEFAULT 1 CHECK (https_only IN (0, 1));
