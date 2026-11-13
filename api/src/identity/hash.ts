import { hmacSha256Hex } from "../crypto";
import type { VerifiedIdentity } from "../stripe/identity-api";

/**
 * The identity hash: HMAC-SHA256, keyed with IDENTITY_HASH_KEY, of the
 * document's issuing country, number and the holder's birth date. Two
 * members with the same hash are the same person. Null when the document
 * lacks one of the three.
 *
 * The document number is compared after Unicode NFKC normalization, without
 * case, spaces or punctuation, so "C01X 00T47" and "c01x00t47" are the same
 * document. Letters and digits of every script count.
 */
export async function identityHash(key: string, identity: VerifiedIdentity): Promise<string | null> {
  const number = identity.documentNumber?.normalize("NFKC").toUpperCase().replace(/[^\p{L}\p{N}]/gu, "") ?? "";
  const country = identity.issuingCountry?.toUpperCase() ?? "";
  const dob = identity.dateOfBirth;
  if (!number || !/^[A-Z]{2}$/.test(country) || !dob) return null;
  if (!key) throw new Error("IDENTITY_HASH_KEY is not set");

  const birthDate = [String(dob.year).padStart(4, "0"), String(dob.month).padStart(2, "0"), String(dob.day).padStart(2, "0")].join("-");
  return hmacSha256Hex(key, `homeport-identity-v1\n${country}\n${number}\n${birthDate}`);
}
