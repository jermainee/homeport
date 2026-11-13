import { type Balances, MEMBER_ACCOUNT_KINDS } from "./book";

/**
 * How much a debit of `amount` takes from each of the member's accounts:
 * purchased first, then earned_pending, then earned_cashable, so cashable
 * credits stay cashable as long as possible. What the accounts cannot cover
 * is taken from purchased, which then goes negative: usage the gateway
 * metered is always booked.
 */
export function spendOrder(balances: Balances, amount: number): Balances {
  const taken: Balances = { purchased: 0, earned_pending: 0, earned_cashable: 0 };
  let left = amount;
  for (const kind of MEMBER_ACCOUNT_KINDS) {
    const part = Math.min(Math.max(balances[kind], 0), left);
    taken[kind] = part;
    left -= part;
  }
  taken.purchased += left;
  return taken;
}

/** Credits the member can still commit: their balance minus what open reservations hold. */
export function availableCredits(balances: Balances, reserved: number): number {
  return balances.purchased + balances.earned_pending + balances.earned_cashable - reserved;
}
