import type Stripe from "stripe";
import { grantCredits } from "../ledger";

const COMPLETED = "checkout.session.completed";
const ASYNC_SUCCEEDED = "checkout.session.async_payment_succeeded";
const ASYNC_FAILED = "checkout.session.async_payment_failed";
const EXPIRED = "checkout.session.expired";

const CHECKOUT_EVENTS: ReadonlySet<string> = new Set([COMPLETED, ASYNC_SUCCEEDED, ASYNC_FAILED, EXPIRED]);

export function isCheckoutEvent(type: string): boolean {
  return CHECKOUT_EVENTS.has(type);
}

interface CheckoutSessionFields {
  id?: unknown;
  metadata?: Record<string, string> | null;
  payment_status?: unknown;
  amount_total?: unknown;
}

/**
 * Applies one Stripe Checkout event to its payment and returns the
 * statements that record it, for the webhook's batch.
 *
 * - `completed` with payment_status `paid`, and `async_payment_succeeded`,
 *   grant the payment's credits into the member's purchased account. The
 *   grant books before the statements run; its idempotency key
 *   'purchase:<session id>' makes every later grant for the session a
 *   duplicate, so a session never grants twice, whatever events arrive.
 * - `completed` while unpaid (a delayed payment method) marks it processing.
 * - `async_payment_failed` and `expired` mark an unpaid payment failed or
 *   expired; a paid one stays paid.
 *
 * An event of a session Homeport did not create for that member changes
 * nothing. The credits come from the payments row, written when Homeport
 * created the session, not from the event.
 */
export async function checkoutEventStatements(db: D1Database, event: Stripe.Event): Promise<D1PreparedStatement[]> {
  const session = event.data.object as CheckoutSessionFields;
  const memberId = session.metadata?.["member_id"];
  if (typeof session.id !== "string" || typeof memberId !== "string") return [];
  const payment = await db
    .prepare(`SELECT credits FROM payments WHERE id = ? AND member_id = ?`)
    .bind(session.id, memberId)
    .first<{ credits: number }>();
  if (!payment) return [];
  const now = Date.now();

  const paid =
    (event.type === COMPLETED || event.type === ASYNC_SUCCEEDED) && session.payment_status === "paid";
  if (paid) {
    const grant = await grantCredits(db, {
      source: "credits_issued",
      memberId,
      credits: payment.credits,
      idempotencyKey: `purchase:${session.id}`,
      now,
    });
    const amountTotal = typeof session.amount_total === "number" ? session.amount_total : null;
    return [
      db
        .prepare(
          `UPDATE payments SET status = 'paid', transaction_id = ?, amount_total_cents = COALESCE(?, amount_total_cents),
                  updated_at = ?
            WHERE id = ?`,
        )
        .bind(grant.transactionId, amountTotal, now, session.id),
    ];
  }

  const status = event.type === COMPLETED ? "processing" : event.type === ASYNC_FAILED ? "failed" : event.type === EXPIRED ? "expired" : null;
  if (status === null) return [];
  return [
    db
      .prepare(
        `UPDATE payments SET status = ?, updated_at = ?
          WHERE id = ? AND status IN ('open', 'processing')`,
      )
      .bind(status, now, session.id),
  ];
}
