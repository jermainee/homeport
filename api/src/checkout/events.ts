import type Stripe from "stripe";
import { grantCredits } from "../ledger";

const COMPLETED = "checkout.session.completed";
const ASYNC_SUCCEEDED = "checkout.session.async_payment_succeeded";
const ASYNC_FAILED = "checkout.session.async_payment_failed";
const EXPIRED = "checkout.session.expired";

/** What an event that grants nothing makes of a payment that is not paid yet. */
const STATUS_BY_EVENT: Readonly<Record<string, "processing" | "failed" | "expired">> = {
  // Only while unpaid: a delayed payment method has not cleared yet.
  [COMPLETED]: "processing",
  [ASYNC_FAILED]: "failed",
  [EXPIRED]: "expired",
};

export function isCheckoutEvent(type: string): boolean {
  return type === ASYNC_SUCCEEDED || type in STATUS_BY_EVENT;
}

interface CheckoutSessionFields {
  id?: unknown;
  metadata?: Record<string, string> | null;
  payment_status?: unknown;
  amount_total?: unknown;
  total_details?: { amount_tax?: unknown } | null;
}

/**
 * Applies one Stripe Checkout event to its payment and returns the
 * statements that record it, for the webhook's batch.
 *
 * - `completed` with payment_status `paid`, and `async_payment_succeeded`,
 *   grant the payment's credits into the member's purchased account and
 *   mark the payment paid, in one batch. The grant's idempotency key
 *   'purchase:<session id>' makes every later grant for the session a
 *   duplicate, so a session never grants twice, whatever events arrive.
 * - `completed` with payment_status `unpaid` (a delayed payment method)
 *   marks it processing. `no_payment_required` changes nothing: every pack
 *   has a price, so Homeport never sells one for nothing.
 * - `async_payment_failed` and `expired` mark an unpaid payment failed or
 *   expired; a paid one stays paid.
 *
 * An event of a session Homeport did not create for that member changes
 * nothing. The credits come from the payments row, written when Homeport
 * created the session, not from the event.
 */
export async function checkoutEventStatements(db: D1Database, event: Stripe.Event): Promise<D1PreparedStatement[]> {
  const session = event.data.object as CheckoutSessionFields;
  const sessionId = session.id;
  const memberId = session.metadata?.["member_id"];
  if (typeof sessionId !== "string" || typeof memberId !== "string") return [];
  const payment = await db
    .prepare(`SELECT credits FROM payments WHERE id = ? AND member_id = ?`)
    .bind(sessionId, memberId)
    .first<{ credits: number }>();
  if (!payment) return [];
  const now = Date.now();

  if ((event.type === COMPLETED || event.type === ASYNC_SUCCEEDED) && session.payment_status === "paid") {
    const amountTotal = typeof session.amount_total === "number" ? session.amount_total : null;
    const amountTax = typeof session.total_details?.amount_tax === "number" ? session.total_details.amount_tax : null;
    // The payment is marked in the grant's batch, so a duplicate grant finds it marked already.
    await grantCredits(db, {
      source: "credits_issued",
      memberId,
      credits: payment.credits,
      idempotencyKey: `purchase:${sessionId}`,
      now,
      alsoWrite: (guard, transactionId) => [
        db
          .prepare(
            `UPDATE payments SET status = 'paid', transaction_id = ?, amount_total_cents = ?, amount_tax_cents = ?, updated_at = ?
              WHERE id = ? AND ${guard.sql}`,
          )
          .bind(transactionId, amountTotal, amountTax, now, sessionId, ...guard.binds),
      ],
    });
    return [];
  }

  const status = STATUS_BY_EVENT[event.type];
  if (status === undefined || (event.type === COMPLETED && session.payment_status !== "unpaid")) return [];
  return [
    db
      .prepare(
        `UPDATE payments SET status = ?, updated_at = ?
          WHERE id = ? AND status IN ('open', 'processing')`,
      )
      .bind(status, now, sessionId),
  ];
}
