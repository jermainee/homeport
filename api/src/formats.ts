/** The contract's scalar formats (contracts/control-api.openapi.yaml, components.schemas). */

/** Id: a lowercase UUID. */
export const ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** UuidV7: created by the sender; its timestamp orders and expires it. */
export const UUID_V7 = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

/** CountryCode: an ISO 3166-1 alpha-2 code. */
export const COUNTRY = /^[A-Z]{2}$/;

/** A city as requests name it: 1 to 200 characters, counted in code points as the contract's maxLength does. */
export function isCity(value: unknown): value is string {
  if (typeof value !== "string") return false;
  const characters = Array.from(value).length;
  return characters >= 1 && characters <= 200;
}

/** Sha256Hex: 64 lowercase hex characters. */
export const SHA256_HEX = /^[0-9a-f]{64}$/;

/** HostPort: `host:port`, with an IPv6 host in brackets. */
export const HOST_PORT = /^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+):[0-9]{1,5}$/;

/** A DNS hostname (JSON Schema format "hostname"). */
export const HOSTNAME = /^(?=.{1,253}$)[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$/;

/** Version: a semantic version. */
export const SEMVER = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$/;

const DATE_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/i;

/** Milliseconds since the Unix epoch, from the first 48 bits of a UUIDv7. */
export function uuidV7Timestamp(id: string): number {
  return parseInt(id.slice(0, 8) + id.slice(9, 13), 16);
}

/** An RFC 3339 date-time (Timestamp) as epoch milliseconds, or null. */
export function parseDateTime(value: unknown): number | null {
  if (typeof value !== "string" || !DATE_TIME.test(value)) return null;
  const ms = Date.parse(value);
  return Number.isFinite(ms) ? ms : null;
}

const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;

/** True for an IPv4 or IPv6 address (IpAddress). */
export function isIpAddress(value: unknown): value is string {
  if (typeof value !== "string") return false;
  if (IPV4.test(value)) return true;
  // The URL parser checks IPv6 syntax; a zone id or brackets are not addresses.
  return /^[0-9A-Fa-f:.]+$/.test(value) && value.includes(":") && URL.canParse(`http://[${value}]/`);
}

/** A whole number from 0 up to the largest safe integer. */
export function isCount(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

/** Epoch milliseconds as the contract writes timestamps: RFC 3339 in UTC, whole seconds. */
export function timestamp(ms: number): string {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
}
