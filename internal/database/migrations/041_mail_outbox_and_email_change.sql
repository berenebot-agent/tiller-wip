-- Durable transactional-mail outbox and email-change confirmation tokens.
--
-- Both tables are platform-global identity infrastructure (like users and
-- email_verification_tokens), so they carry no account_id and are ClassPlatform
-- in internal/database/classification.go.
--
-- mail_outbox replaces best-effort delivery: signup/verification/password-reset/
-- email-change enqueue inside the same transaction that creates the one-time
-- token, then a background worker delivers with bounded retries. The raw
-- one-time token in token_ciphertext is encrypted at rest with the always-on
-- master key (internal/crypto, enc:v1:), so a core backup leak cannot yield an
-- account takeover. token_ciphertext is NULLed on successful send; recipient and
-- params stay readable for operator triage.
--
-- email_change_tokens stores the pending new address (plain text: it is not a
-- secret) and only a hash of the confirming token. keep_session_id records the
-- session selector that initiated the change so confirmation can revoke every
-- other session while keeping the initiating one.
CREATE TABLE mail_outbox (
  id TEXT PRIMARY KEY,
  user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
  type TEXT NOT NULL,
  recipient TEXT NOT NULL,
  params TEXT NOT NULL DEFAULT '{}',
  token_ciphertext TEXT,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  sent_at TEXT,
  last_error TEXT,
  dead_at TEXT
) STRICT;
-- Partial index over live work only: the worker scans sent_at IS NULL AND
-- dead_at IS NULL ordered by due time. Completed/dead rows leave the index.
CREATE INDEX mail_outbox_due ON mail_outbox(next_attempt_at, created_at)
  WHERE sent_at IS NULL AND dead_at IS NULL;
CREATE INDEX mail_outbox_user ON mail_outbox(user_id);

CREATE TABLE email_change_tokens (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  new_email TEXT NOT NULL,
  token_hash TEXT NOT NULL,
  keep_session_id TEXT,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used_at TEXT
) STRICT;
CREATE INDEX email_change_tokens_user ON email_change_tokens(user_id);
CREATE INDEX email_change_tokens_expires ON email_change_tokens(expires_at);
