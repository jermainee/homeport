/** The Stripe API version every call and every webhook endpoint uses. */
export const STRIPE_API_VERSION = "2026-10-28.endive";

/** `value`, or an error naming the missing secret. */
export function requireKey(value: string | undefined, name: string): string {
  if (!value) throw new Error(`${name} is not set`);
  return value;
}
