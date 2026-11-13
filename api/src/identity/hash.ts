import type { VerifiedIdentity } from "../stripe/identity-api";

const encoder = new TextEncoder();

/**
 * The identity hash: HMAC-SHA256, keyed with IDENTITY_HASH_KEY, of the
 * document's issuing country, number and the holder's birth date. Two
 * members with the same hash are the same person. Null when the document
 * lacks one of the three.
 *
 * The document number is compared without case, spaces or punctuation, so
 * "C01X 00T47" and "c01x00t47" are the same document.
 */
export async function identityHash(key: string, identity: VerifiedIdentity): Promise<string | null> {
  const number = identity.documentNumber?.toUpperCase().replace(/[^0-9A-Z]/g, "") ?? "";
  const country = identity.issuingCountry?.toUpperCase() ?? "";
  const dob = identity.dateOfBirth;
  if (!number || !/^[A-Z]{2}$/.test(country) || !dob) return null;
  if (!key) throw new Error("IDENTITY_HASH_KEY is not set");

  const birthDate = `${String(dob.year).padStart(4, "0")}-${String(dob.month).padStart(2, "0")}-${String(dob.day).padStart(2, "0")}`;
  const hmacKey = await crypto.subtle.importKey("raw", encoder.encode(key), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const mac = await crypto.subtle.sign("HMAC", hmacKey, encoder.encode(`homeport-identity-v1\n${country}\n${number}\n${birthDate}`));
  return Array.from(new Uint8Array(mac), (byte) => byte.toString(16).padStart(2, "0")).join("");
}
