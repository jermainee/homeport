/** A 32-byte public key in standard base64, as WireGuard writes keys (contract schema Key32). */
export const KEY32 = /^[A-Za-z0-9+/]{42}[AEIMQUYcgkosw480]=$/;

/** DER prefix of an Ed25519 SubjectPublicKeyInfo (RFC 8410); the raw 32-byte key follows. */
const ED25519_SPKI_PREFIX = Uint8Array.from([0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00]);

/**
 * SHA-256 (lowercase hex) of the SubjectPublicKeyInfo of a raw Ed25519 key
 * given in base64: what the gateway compares during mutual TLS.
 */
export async function ed25519SpkiSha256(publicKeyBase64: string): Promise<string> {
  const key = Uint8Array.from(atob(publicKeyBase64), (char) => char.charCodeAt(0));
  if (key.length !== 32) throw new RangeError("an Ed25519 public key has 32 bytes");
  const spki = new Uint8Array(ED25519_SPKI_PREFIX.length + key.length);
  spki.set(ED25519_SPKI_PREFIX);
  spki.set(key, ED25519_SPKI_PREFIX.length);
  const digest = await crypto.subtle.digest("SHA-256", spki);
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

/**
 * A string of 1 to 64 characters, as the contract's device and peer names.
 * Characters are code points, as JSON Schema counts them, not UTF-16 units.
 */
export function isName(value: unknown): value is string {
  if (typeof value !== "string") return false;
  // Counts code points on purpose: the contract's maxLength does, not grapheme clusters.
  const characters = Array.from(value).length;
  return characters >= 1 && characters <= 64;
}
