-- Stripe Identity: verification sessions, the identity hash and processed
-- Stripe webhook events. Code: src/identity/, src/stripe/.
-- Timestamps are Unix epoch milliseconds (INTEGER).

-- HMAC-SHA256 (key IDENTITY_HASH_KEY) of issuing country, document number and
-- birth date from a verified live-mode document. One person, one account:
-- no two members share it. NULL until verified, and for test-mode checks.
ALTER TABLE members ADD COLUMN identity_hash TEXT;
-- Why the last check did not verify the member, when Homeport decided it
-- (for example 'duplicate_identity'). Internal; members see requires_input.
ALTER TABLE members ADD COLUMN identity_failure TEXT;
-- `created` (epoch seconds) of the newest Stripe Identity event applied to
-- the member, so an older event that arrives late changes nothing.
ALTER TABLE members ADD COLUMN identity_event_created INTEGER;

CREATE UNIQUE INDEX members_identity_hash ON members (identity_hash) WHERE identity_hash IS NOT NULL;

-- Verification sessions Homeport created, so a webhook event is applied
-- only to the member who started the session.
CREATE TABLE identity_sessions (
  -- Stripe's VerificationSession id (vs_...).
  id TEXT PRIMARY KEY,
  member_id TEXT NOT NULL REFERENCES members (id),
  created_at INTEGER NOT NULL
);

CREATE INDEX identity_sessions_member ON identity_sessions (member_id);

-- Stripe webhook events already handled. Stripe may deliver an event more
-- than once; a second delivery changes nothing.
CREATE TABLE stripe_events (
  -- Stripe's event id (evt_...).
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  livemode INTEGER NOT NULL,
  processed_at INTEGER NOT NULL
);
