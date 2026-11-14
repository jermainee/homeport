/**
 * Every price, billing constant and product limit Homeport uses. All other code reads them
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

/** Names the credit pack in Stripe metadata and in payments. A different pack gets a new id. */
export const CREDIT_PACK_ID = "credits_1000000";

/** Credits in one credit pack. */
export const CREDIT_PACK_CREDITS = 1_000_000;

/** Net price of one credit pack in euro cents (€40). Stripe Tax adds tax at checkout. */
export const CREDIT_PACK_NET_CENTS = 4_000;

export const CREDIT_PACK_CURRENCY = "eur";

/**
 * Stripe product tax code for credit packs: General - Electronically Supplied
 * Services (https://docs.stripe.com/tax/tax-codes). Chosen by the owner on
 * 2026-11-14; confirm with a tax advisor before selling in the US, where a more
 * specific code may apply.
 */
export const CREDIT_PACK_TAX_CODE = "txcd_10000000";

/**
 * Checkout Sessions one member may open per CHECKOUT_SESSION_WINDOW_MS.
 * Each one calls Stripe, whose API rate limit all members share.
 */
export const MAX_CHECKOUT_SESSIONS_PER_WINDOW = 10;

export const CHECKOUT_SESSION_WINDOW_MS = 60 * 60 * 1000;

/** A shared line serves at most this many members besides its owner. */
export const MAX_USERS_PER_LINE = 3;

/**
 * Active WireGuard peers one member may have. All peers share one pool of
 * tunnel addresses, so without a cap one account could use it up.
 */
export const MAX_PEERS_PER_MEMBER = 20;

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
 * How long a lease lasts if the session neither uses it up nor returns it.
 * Its credits stay reserved until then.
 */
export const LEASE_TTL_SECONDS = 60 * 60;

/** What one lease reserves and grants. */
export interface LeaseTerms {
  credits: number;
  grantedBytes: number;
  renewAtBytes: number;
}

/**
 * The lease for `credits` reserved credits: the bytes they buy at the use
 * rate, rounded down, renewed at LEASE_RENEW_PERCENT. A full lease reserves
 * LEASE_CREDITS; a member with fewer left gets a smaller last lease.
 */
export function leaseTerms(credits: number): LeaseTerms {
  if (!Number.isSafeInteger(credits) || credits <= 0 || credits > LEASE_CREDITS) {
    throw new RangeError(`a lease reserves 1 to ${LEASE_CREDITS} credits`);
  }
  const grantedBytes = Number((BigInt(credits) * BigInt(BYTES_PER_MB)) / BigInt(USE_CREDITS_PER_MB));
  return { credits, grantedBytes, renewAtBytes: Math.floor((grantedBytes * LEASE_RENEW_PERCENT) / 100) };
}

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
