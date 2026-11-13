-- Credits ledger: double-entry, append-only, whole credits.
-- Code: src/ledger/. Prices: src/pricing.ts. Rules: docs/concept.md.
-- Timestamps are Unix epoch milliseconds (INTEGER).

-- Optimistic lock for member balance changes (src/ledger/guarded-batch.ts).
-- D1 has no interactive transactions, so a change reads ledger_version, then
-- runs one db.batch: its first statement bumps the version, guarded by the
-- value read, and sets ledger_nonce; every later statement runs only while
-- ledger_nonce still holds that attempt's nonce.
ALTER TABLE members ADD COLUMN ledger_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE members ADD COLUMN ledger_nonce TEXT;

CREATE TABLE ledger_accounts (
  -- '<member_id>:<kind>' for a member's accounts, 'platform:<kind>' for the platform's.
  id TEXT PRIMARY KEY,
  -- Member kinds, spent in this order: purchased, earned_pending, earned_cashable.
  -- Platform kinds: platform_revenue, credits_issued (source of sold credits),
  -- dev_grants (source of credits granted in development).
  kind TEXT NOT NULL CHECK (
    kind IN ('purchased', 'earned_pending', 'earned_cashable',
             'platform_revenue', 'credits_issued', 'dev_grants')
  ),
  member_id TEXT REFERENCES members (id),
  -- Cached sum of the account's entries, kept by the ledger_entries_balance trigger.
  balance INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  CHECK ((member_id IS NULL) = (kind IN ('platform_revenue', 'credits_issued', 'dev_grants')))
);

INSERT INTO ledger_accounts (id, kind, member_id, created_at) VALUES
  ('platform:platform_revenue', 'platform_revenue', NULL, 0),
  ('platform:credits_issued', 'credits_issued', NULL, 0),
  ('platform:dev_grants', 'dev_grants', NULL, 0);

-- Every member gets its three accounts when it is created.
CREATE TRIGGER members_ledger_accounts AFTER INSERT ON members
BEGIN
  INSERT INTO ledger_accounts (id, kind, member_id, created_at) VALUES
    (NEW.id || ':purchased', 'purchased', NEW.id, NEW.created_at),
    (NEW.id || ':earned_pending', 'earned_pending', NEW.id, NEW.created_at),
    (NEW.id || ':earned_cashable', 'earned_cashable', NEW.id, NEW.created_at);
END;

-- Members created before this migration.
INSERT INTO ledger_accounts (id, kind, member_id, created_at)
SELECT m.id || ':' || k.kind, k.kind, m.id, m.created_at
  FROM members m
 CROSS JOIN (SELECT 'purchased' AS kind
             UNION ALL SELECT 'earned_pending'
             UNION ALL SELECT 'earned_cashable') k;

-- Only the cached balance of an account ever changes.
CREATE TRIGGER ledger_accounts_fixed BEFORE UPDATE OF id, kind, member_id, created_at ON ledger_accounts
BEGIN
  SELECT RAISE(ABORT, 'ledger accounts never change owner or kind');
END;

CREATE TRIGGER ledger_accounts_no_delete BEFORE DELETE ON ledger_accounts
BEGIN
  SELECT RAISE(ABORT, 'ledger accounts are never deleted');
END;

CREATE TABLE ledger_transactions (
  id TEXT PRIMARY KEY,
  -- The business event this transaction books, for example 'usage:<usage_key>'
  -- or 'dev_grant:<request_id>'. A retry finds it and books nothing.
  idempotency_key TEXT NOT NULL UNIQUE,
  -- dev_grant | usage (later also purchase, reversal, ...)
  kind TEXT NOT NULL,
  -- The member whose ledger_version guarded the batch that wrote it.
  member_id TEXT REFERENCES members (id),
  created_at INTEGER NOT NULL
);

CREATE TABLE ledger_entries (
  id INTEGER PRIMARY KEY,
  transaction_id TEXT NOT NULL REFERENCES ledger_transactions (id),
  account_id TEXT NOT NULL REFERENCES ledger_accounts (id),
  -- Signed whole credits: positive adds to the account, negative takes from it.
  amount INTEGER NOT NULL CHECK (amount <> 0),
  created_at INTEGER NOT NULL
);

CREATE INDEX ledger_entries_transaction ON ledger_entries (transaction_id);
CREATE INDEX ledger_entries_account ON ledger_entries (account_id);

-- Append-only: a correction is a new, reversing transaction.
CREATE TRIGGER ledger_transactions_no_update BEFORE UPDATE ON ledger_transactions
BEGIN
  SELECT RAISE(ABORT, 'ledger_transactions is append-only');
END;

CREATE TRIGGER ledger_transactions_no_delete BEFORE DELETE ON ledger_transactions
BEGIN
  SELECT RAISE(ABORT, 'ledger_transactions is append-only');
END;

CREATE TRIGGER ledger_entries_no_update BEFORE UPDATE ON ledger_entries
BEGIN
  SELECT RAISE(ABORT, 'ledger_entries is append-only');
END;

CREATE TRIGGER ledger_entries_no_delete BEFORE DELETE ON ledger_entries
BEGIN
  SELECT RAISE(ABORT, 'ledger_entries is append-only');
END;

-- The cached balance moves in the same statement as the entry.
CREATE TRIGGER ledger_entries_balance AFTER INSERT ON ledger_entries
BEGIN
  UPDATE ledger_accounts SET balance = balance + NEW.amount WHERE id = NEW.account_id;
END;

-- Inserting a transaction id into this view checks that the transaction
-- balances: at least two entries that sum to zero. Every booking ends with
-- this insert, so an unbalanced transaction aborts its whole batch. Nothing
-- is stored.
CREATE VIEW ledger_balance_checks AS SELECT NULL AS transaction_id WHERE 0;

CREATE TRIGGER ledger_balance_check INSTEAD OF INSERT ON ledger_balance_checks
BEGIN
  SELECT RAISE(ABORT, 'ledger transaction does not balance')
   WHERE (SELECT COUNT(*) FROM ledger_entries WHERE transaction_id = NEW.transaction_id) < 2
      OR (SELECT SUM(amount) FROM ledger_entries WHERE transaction_id = NEW.transaction_id) <> 0;
END;

-- Running usage totals per meter (for example one WireGuard peer). A charge
-- is floor(total_bytes × rate / 10^6) minus what the meter already charged,
-- so rounding never adds up across usage events.
CREATE TABLE meter_totals (
  meter_id TEXT PRIMARY KEY,
  -- Pays for the usage.
  payer_member_id TEXT NOT NULL REFERENCES members (id),
  -- Owns the line the usage went through, and earns from it.
  payee_member_id TEXT NOT NULL REFERENCES members (id),
  total_bytes INTEGER NOT NULL CHECK (total_bytes >= 0),
  charged_credits INTEGER NOT NULL CHECK (charged_credits >= 0),
  earned_credits INTEGER NOT NULL CHECK (earned_credits >= 0),
  updated_at INTEGER NOT NULL
);

-- A meter belongs to one payer and one payee for good. Usage writes them on
-- every update, so a batch that would move a meter aborts here.
CREATE TRIGGER meter_totals_owners_fixed BEFORE UPDATE OF payer_member_id, payee_member_id ON meter_totals
WHEN NEW.payer_member_id <> OLD.payer_member_id OR NEW.payee_member_id <> OLD.payee_member_id
BEGIN
  SELECT RAISE(ABORT, 'a meter never changes payer or payee');
END;

-- Each piece of usage applied to a meter (for example one gateway usage
-- event), so a retry is never counted twice. These rows are not ledger rows:
-- they may be deleted once a retry of their usage can no longer arrive.
CREATE TABLE usage_charges (
  usage_key TEXT PRIMARY KEY,
  meter_id TEXT NOT NULL REFERENCES meter_totals (meter_id),
  bytes INTEGER NOT NULL CHECK (bytes >= 0),
  charged_credits INTEGER NOT NULL CHECK (charged_credits >= 0),
  earned_credits INTEGER NOT NULL CHECK (earned_credits >= 0),
  -- NULL when the usage did not add up to a whole credit yet.
  transaction_id TEXT REFERENCES ledger_transactions (id),
  created_at INTEGER NOT NULL
);

-- Credits a quota lease holds back. A reservation books nothing: a member's
-- available credits are their balance minus what open reservations still
-- hold (credits - charged_credits, until released or expired).
CREATE TABLE credit_reservations (
  id TEXT PRIMARY KEY,
  member_id TEXT NOT NULL REFERENCES members (id),
  credits INTEGER NOT NULL CHECK (credits > 0),
  -- Usage charged against this reservation so far.
  charged_credits INTEGER NOT NULL DEFAULT 0 CHECK (charged_credits >= 0),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  released_at INTEGER
);

CREATE INDEX credit_reservations_open ON credit_reservations (member_id, expires_at)
  WHERE released_at IS NULL;
