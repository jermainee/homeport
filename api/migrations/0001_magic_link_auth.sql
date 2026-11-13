-- Members, email magic-link sign-in, sessions and request_id idempotency.
-- Timestamps are Unix epoch milliseconds (INTEGER).
-- Tokens are stored only as lowercase hex SHA-256 hashes.

CREATE TABLE members (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL UNIQUE COLLATE NOCASE,
  -- active | suspended
  status TEXT NOT NULL DEFAULT 'active',
  -- unverified | requires_input | processing | verified (set from Stripe Identity in a later PR)
  identity_status TEXT NOT NULL DEFAULT 'unverified',
  created_at INTEGER NOT NULL
);

-- One sign-in attempt. The app creates the session token and sends only its
-- hash; approving the emailed link activates it as an auth session.
CREATE TABLE login_requests (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL COLLATE NOCASE,
  -- A session token belongs to one login request only.
  session_token_hash TEXT NOT NULL UNIQUE,
  -- Shown in the app and on the confirm page; not a secret.
  match_code TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  confirmed_at INTEGER,
  member_id TEXT REFERENCES members (id)
);

CREATE TABLE magic_link_tokens (
  token_hash TEXT PRIMARY KEY,
  login_request_id TEXT NOT NULL REFERENCES login_requests (id),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at INTEGER
);

CREATE INDEX magic_link_tokens_login_request ON magic_link_tokens (login_request_id);

CREATE TABLE auth_sessions (
  token_hash TEXT PRIMARY KEY,
  member_id TEXT NOT NULL REFERENCES members (id),
  install_id TEXT,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  revoked_at INTEGER
);

CREATE INDEX auth_sessions_member ON auth_sessions (member_id);

-- The first response to each POST request_id, replayed to retries with the
-- same body. Responses never carry a secret. The fingerprint is a SHA-256 of
-- method, path and body, so no request body is stored.
CREATE TABLE idempotency_records (
  request_id TEXT PRIMARY KEY,
  fingerprint TEXT NOT NULL,
  -- NULL while the first request is still running.
  response_status INTEGER,
  response_body TEXT,
  -- When a request last claimed this id; a claim silent for a minute is taken over.
  claimed_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE INDEX idempotency_records_expires ON idempotency_records (expires_at);
