-- Credit packs bought through Stripe Checkout. Code: src/checkout/,
-- src/stripe/checkout-api.ts. Prices: src/pricing.ts.
-- Timestamps are Unix epoch milliseconds (INTEGER).

-- One Checkout Session Homeport created. The row is written when the
-- session is created and fixes what a payment grants, so a webhook event
-- grants only what Homeport sold, only to the member who opened the
-- session, and at most once: the grant's ledger transaction has the
-- idempotency key 'purchase:<id>'.
CREATE TABLE payments (
  -- Stripe's Checkout Session id (cs_...).
  id TEXT PRIMARY KEY,
  member_id TEXT NOT NULL REFERENCES members (id),
  -- The credit pack sold (pricing.ts CREDIT_PACK_ID).
  pack TEXT NOT NULL,
  -- Credits the payment grants into the member's purchased account.
  credits INTEGER NOT NULL CHECK (credits > 0),
  -- Net price in integer cents; VAT is added on top at checkout.
  net_cents INTEGER NOT NULL CHECK (net_cents > 0),
  -- ISO 4217, lowercase as Stripe writes it.
  currency TEXT NOT NULL,
  -- open: created, not paid yet
  -- processing: completed, but a delayed payment method has not cleared yet
  -- paid: the credits are granted (transaction_id)
  -- failed: the delayed payment failed
  -- expired: the session expired unpaid
  status TEXT NOT NULL DEFAULT 'open'
    CHECK (status IN ('open', 'processing', 'paid', 'failed', 'expired')),
  -- What Stripe charged, VAT included, in integer cents; set when paid.
  amount_total_cents INTEGER,
  -- The ledger transaction that granted the credits.
  transaction_id TEXT REFERENCES ledger_transactions (id),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  CHECK ((status = 'paid') = (transaction_id IS NOT NULL))
);

CREATE INDEX payments_member ON payments (member_id, created_at);
