-- Hosted user identity, sessions, and one-time tokens.
--
-- These tables are platform-global (docs/sass_tech.md section 6.1): a user is not
-- owned by an account; the account is owned by the user. They are classified
-- ClassPlatform in internal/database/classification.go.
--
-- Email is the identity and is stored normalized (trimmed, lowercased), so
-- UNIQUE(email) is a case-insensitive uniqueness constraint. Passwords are
-- stored as Argon2id PHC strings. Verification/reset tokens store only a hash
-- and a selector; the raw token never reaches the database.
CREATE TABLE users (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  email_verified_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE user_sessions (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL,
  csrf_token TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  last_used_at TEXT NOT NULL
) STRICT;
CREATE INDEX user_sessions_expires ON user_sessions(expires_at);
CREATE INDEX user_sessions_user ON user_sessions(user_id);
CREATE INDEX user_sessions_account ON user_sessions(account_id);

CREATE TABLE email_verification_tokens (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used_at TEXT
) STRICT;
CREATE INDEX email_verification_tokens_user ON email_verification_tokens(user_id);
CREATE INDEX email_verification_tokens_expires ON email_verification_tokens(expires_at);

CREATE TABLE password_reset_tokens (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used_at TEXT
) STRICT;
CREATE INDEX password_reset_tokens_user ON password_reset_tokens(user_id);
CREATE INDEX password_reset_tokens_expires ON password_reset_tokens(expires_at);

CREATE TABLE platform_admin_sessions (
  id TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL,
  csrf_token TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  last_used_at TEXT NOT NULL
) STRICT;
CREATE INDEX platform_admin_sessions_expires ON platform_admin_sessions(expires_at);
