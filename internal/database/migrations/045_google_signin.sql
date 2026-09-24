-- Hosted Google sign-in is a platform identity method attached to a user.
-- Passwordless Google-first users have password_auth_enabled=0 and an empty
-- password_hash; existing password accounts retain their current hashes.
ALTER TABLE users ADD COLUMN password_auth_enabled INTEGER NOT NULL DEFAULT 1 CHECK (password_auth_enabled IN (0,1));

CREATE TABLE user_identities (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider TEXT NOT NULL CHECK (provider='google'),
  subject TEXT NOT NULL,
  email TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(provider, subject),
  UNIQUE(user_id, provider)
) STRICT;
CREATE INDEX user_identities_user ON user_identities(user_id);
