-- Migration number: 0007 	 2026-11-14T00:00:00.000Z
-- Stripe Tax adds tax on top of each credit pack's net price. Keep the tax
-- Stripe collected apart from the total, so the VAT a payment owes can be
-- booked from Homeport's own records. Set when the payment is paid.
ALTER TABLE payments ADD COLUMN amount_tax_cents INTEGER;
