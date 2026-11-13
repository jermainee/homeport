const encoder = new TextEncoder();

/** A random secret with `bytes` bytes of entropy, base64url-encoded without padding. */
export function randomToken(bytes = 32): string {
  return base64url(crypto.getRandomValues(new Uint8Array(bytes)));
}

/** Lowercase hex SHA-256 of a string. Tokens and secrets are stored only in this form. */
export async function sha256Hex(value: string): Promise<string> {
  return hex(await crypto.subtle.digest("SHA-256", encoder.encode(value)));
}

/** Lowercase hex HMAC-SHA256 of `message` under `key`. */
export async function hmacSha256Hex(key: string, message: string): Promise<string> {
  const hmacKey = await crypto.subtle.importKey("raw", encoder.encode(key), { name: "HMAC", hash: "SHA-256" }, false, [
    "sign",
  ]);
  return hex(await crypto.subtle.sign("HMAC", hmacKey, encoder.encode(message)));
}

function hex(bytes: ArrayBuffer): string {
  return Array.from(new Uint8Array(bytes), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

/** A uniformly random six-digit code, such as "042917". */
export function randomMatchCode(): string {
  // 4,294,000,000 is the largest multiple of 10^6 below 2^32; rejecting values
  // at or above it keeps every code equally likely.
  const limit = 4_294_000_000;
  const buffer = new Uint32Array(1);
  for (;;) {
    crypto.getRandomValues(buffer);
    const value = buffer[0] ?? limit;
    if (value < limit) return String(value % 1_000_000).padStart(6, "0");
  }
}

function base64url(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}
