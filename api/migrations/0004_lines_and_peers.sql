-- Devices (sharing agents), lines, WireGuard peers, gateways and the member
-- links that rule out self-dealing. Code: src/devices.ts, src/lines/,
-- src/wireguard/. Timestamps are Unix epoch milliseconds (INTEGER).

-- Edge gateways as they announce themselves (PUT /v1/gateway/self, later).
CREATE TABLE gateways (
  id TEXT PRIMARY KEY,
  wg_public_key TEXT NOT NULL,
  -- host:port where WireGuard peers connect.
  wg_endpoint TEXT NOT NULL,
  -- host:port where sharing agents connect over QUIC.
  quic_endpoint TEXT NOT NULL,
  quic_server_name TEXT NOT NULL,
  -- SHA-256 of the gateway certificate's SubjectPublicKeyInfo; agents pin it.
  quic_spki_sha256 TEXT NOT NULL,
  version TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- A sharing agent's Ed25519 key, used for mutual TLS with the gateway.
CREATE TABLE devices (
  id TEXT PRIMARY KEY,
  member_id TEXT NOT NULL REFERENCES members (id),
  name TEXT NOT NULL,
  -- linux | macos | windows
  platform TEXT NOT NULL,
  -- The raw 32-byte public key in standard base64. One member per key.
  agent_public_key TEXT NOT NULL UNIQUE,
  -- SHA-256 (hex) of the key's SubjectPublicKeyInfo (DER); the gateway checks it.
  spki_sha256 TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL
);

CREATE INDEX devices_member ON devices (member_id);

-- The home line a device shares. One line per device.
CREATE TABLE lines (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL UNIQUE REFERENCES devices (id),
  member_id TEXT NOT NULL REFERENCES members (id),
  gateway_id TEXT NOT NULL REFERENCES gateways (id),
  sharing_enabled INTEGER NOT NULL DEFAULT 0,
  -- Where the line is, from Cloudflare's view of the request that created
  -- it (the device is at home then). Line tests replace them later.
  country TEXT,
  city TEXT,
  isp TEXT,
  ipv6 INTEGER NOT NULL DEFAULT 0,
  -- Whether the line's agent is connected to its gateway (line events, later).
  online INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX lines_member ON lines (member_id);
CREATE INDEX lines_sharing ON lines (sharing_enabled, created_at);

-- A device that uses a line through WireGuard. Deleted peers stay (with
-- deleted_at) so late usage still finds its member; their key and address
-- become free again.
CREATE TABLE wireguard_peers (
  id TEXT PRIMARY KEY,
  member_id TEXT NOT NULL REFERENCES members (id),
  line_id TEXT NOT NULL REFERENCES lines (id),
  name TEXT NOT NULL,
  -- WireGuard public key, standard base64. The private key never reaches the API.
  public_key TEXT NOT NULL,
  -- Host number in both tunnel networks: 10.77.0.0/16 and fd77::/64.
  -- 1 is the gateway (10.77.0.1), so peers get 2 to 65534.
  address_index INTEGER NOT NULL CHECK (address_index BETWEEN 2 AND 65534),
  created_at INTEGER NOT NULL,
  deleted_at INTEGER
);

CREATE UNIQUE INDEX wireguard_peers_public_key ON wireguard_peers (public_key) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX wireguard_peers_address ON wireguard_peers (address_index) WHERE deleted_at IS NULL;
CREATE INDEX wireguard_peers_member ON wireguard_peers (member_id, created_at);
CREATE INDEX wireguard_peers_line ON wireguard_peers (line_id) WHERE deleted_at IS NULL;

-- Members linked to each other, for example by household, device, payment
-- card or Connect account. A member never uses a linked member's line, in
-- either direction.
CREATE TABLE member_links (
  member_id TEXT NOT NULL REFERENCES members (id),
  linked_member_id TEXT NOT NULL REFERENCES members (id),
  -- household | device | card | connect_account
  reason TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (member_id, linked_member_id, reason),
  CHECK (member_id <> linked_member_id)
);

CREATE INDEX member_links_linked ON member_links (linked_member_id);

-- Members who signed in from the same app install are linked too.
CREATE INDEX auth_sessions_install ON auth_sessions (install_id) WHERE install_id IS NOT NULL;
