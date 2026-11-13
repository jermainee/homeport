/**
 * Every price and billing constant Homeport uses. All other code reads them
 * from here. The reasoning behind the values is in docs/concept.md
 * ("Business model and unit economics"); a change here updates that file and
 * the rules in AGENTS.md in the same pull request.
 *
 * Credits are integers. Euros appear only at purchase and cash-out, in
 * integer cents.
 */

/** 1 MB. Usage counts upload plus download. */
export const BYTES_PER_MB = 1_000_000;

/** What a member pays per MB used through someone else's line. */
export const USE_CREDITS_PER_MB = 10;

/** What a line's owner earns per MB served. The platform keeps the difference. */
export const SHARE_CREDITS_PER_MB = 5;

/** What an exclusive line costs its user per day. */
export const EXCLUSIVE_LINE_USER_CREDITS_PER_DAY = 10_000;

/** What an exclusive line earns its owner per day. */
export const EXCLUSIVE_LINE_OWNER_CREDITS_PER_DAY = 5_000;

/** Credits in one credit pack. */
export const CREDIT_PACK_CREDITS = 1_000_000;

/** Net price of one credit pack in euro cents (€40). VAT is added at checkout. */
export const CREDIT_PACK_NET_CENTS = 4_000;

export const CREDIT_PACK_CURRENCY = "eur";

/** Days before earned credits can be cashed out. */
export const EARNED_CREDITS_HOLDBACK_DAYS = 30;

/** Credits one quota lease reserves from the member's balance. */
export const LEASE_CREDITS = 2_000;

/** Bytes one lease lets a session use: LEASE_CREDITS at the use rate (200 MB). */
export const LEASE_BYTES = wholeQuotient(LEASE_CREDITS * BYTES_PER_MB, USE_CREDITS_PER_MB);

/** The gateway asks for the next lease once a session has used this share of the current one. */
export const LEASE_RENEW_PERCENT = 80;

/** Bytes into a lease at which the gateway asks for the next one (160 MB). */
export const LEASE_RENEW_AT_BYTES = wholeQuotient(LEASE_BYTES * LEASE_RENEW_PERCENT, 100);

/** How long a session stays open, top-up page only, once no next lease is granted. */
export const LEASE_GRACE_SECONDS = 300;

/**
 * Credits for a byte total at a per-MB rate, rounded down:
 * floor(totalBytes × creditsPerMb / 10^6).
 *
 * Charges apply this to a meter's running total and subtract what was
 * already charged, so rounding never adds up across usage events. The math
 * runs in BigInt, so it stays exact for any safe-integer total.
 */
export function creditsForBytes(totalBytes: number, creditsPerMb: number): number {
  if (!Number.isSafeInteger(totalBytes) || totalBytes < 0) {
    throw new RangeError("totalBytes must be a non-negative safe integer");
  }
  if (!Number.isSafeInteger(creditsPerMb) || creditsPerMb < 0) {
    throw new RangeError("creditsPerMb must be a non-negative safe integer");
  }
  return Number((BigInt(totalBytes) * BigInt(creditsPerMb)) / BigInt(BYTES_PER_MB));
}

/** `dividend / divisor`, which must be a whole number: byte counts the gateway receives are integers. */
function wholeQuotient(dividend: number, divisor: number): number {
  const quotient = dividend / divisor;
  if (!Number.isSafeInteger(quotient)) throw new RangeError(`${dividend} / ${divisor} is not a whole number`);
  return quotient;
}
