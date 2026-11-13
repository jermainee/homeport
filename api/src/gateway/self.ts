import { HOSTNAME, HOST_PORT, SEMVER, SHA256_HEX } from "../formats";
import { hasOnlyFields } from "../http";
import { KEY32 } from "../keys";

/** A gateway's endpoints and keys (contract schema GatewaySelf). */
export interface GatewaySelf {
  wg_public_key: string;
  wg_endpoint: string;
  quic_endpoint: string;
  quic_server_name: string;
  quic_spki_sha256: string;
  version: string;
}

const CHECKS: Readonly<Record<keyof GatewaySelf, RegExp>> = {
  wg_public_key: KEY32,
  wg_endpoint: HOST_PORT,
  quic_endpoint: HOST_PORT,
  quic_server_name: HOSTNAME,
  quic_spki_sha256: SHA256_HEX,
  version: SEMVER,
};

const FIELDS = Object.keys(CHECKS) as (keyof GatewaySelf)[];

/** The body as a GatewaySelf, or null if it has a missing, unknown or malformed field. */
export function parseGatewaySelf(body: Record<string, unknown> | null): GatewaySelf | null {
  if (!body || !hasOnlyFields(body, FIELDS)) return null;
  const self: Partial<GatewaySelf> = {};
  for (const field of FIELDS) {
    const value = body[field];
    if (typeof value !== "string" || !CHECKS[field].test(value)) return null;
    self[field] = value;
  }
  return self as GatewaySelf;
}

/** Stores a gateway's announcement, replacing the previous one. */
export function storeGatewaySelf(db: D1Database, gatewayId: string, self: GatewaySelf, now: number): D1PreparedStatement {
  return db
    .prepare(
      `INSERT INTO gateways (id, wg_public_key, wg_endpoint, quic_endpoint, quic_server_name, quic_spki_sha256, version, updated_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT (id) DO UPDATE SET
         wg_public_key = excluded.wg_public_key, wg_endpoint = excluded.wg_endpoint,
         quic_endpoint = excluded.quic_endpoint, quic_server_name = excluded.quic_server_name,
         quic_spki_sha256 = excluded.quic_spki_sha256, version = excluded.version, updated_at = excluded.updated_at`,
    )
    .bind(
      gatewayId,
      self.wg_public_key,
      self.wg_endpoint,
      self.quic_endpoint,
      self.quic_server_name,
      self.quic_spki_sha256,
      self.version,
      now,
    );
}
