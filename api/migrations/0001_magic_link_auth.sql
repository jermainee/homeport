-- Members, email magic-link sign-in and sessions.
-- Timestamps are Unix epoch milliseconds (INTEGER).
-- Tokens and secrets are stored only as lowercase hex SHA-256 hashes.

CREATE TABLE members (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL UNIQUE COLLATE NOCASE,
  -- active | suspended
  status TEXT NOT NULL DEFAULT 'active',
  -- unverified | processing | requires_input | verified (set by Stripe Identity in a later migration)
  identity_status TEXT NOT NULL DEFAULT 'unverified',
  created_at INTEGER NOT NULL
);

-- One sign-in attempt, started by an app and confirmed from the emailed link.
CREATE TABLE login_requests (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL COLLATE NOCASE,
  poll_secret_hash TEXT NOT NULL,
  -- Shown in the app and on the confirm page; not a secret.
  match_code TEXT NOT NULL,
  install_id TEXT,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  confirmed_at INTEGER,
  member_id TEXT REFERENCES members (id),
  -- Set when a poll hands out the session token, so it is handed out once.
  completed_at INTEGER
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
