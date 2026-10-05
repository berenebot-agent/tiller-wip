-- WebAuthn passkeys. A passkey is an alternate credential bound to a user,
-- alongside the password (users.password_hash with password_auth_enabled) and
-- the Google identity (user_identities). A user may register many passkeys, so
-- they live in their own table rather than user_identities (which is unique per
-- provider).
--
-- credential_id is the authenticator's opaque handle and is looked up on every
-- login, so it is UNIQUE across the installation. public_key is the COSE public
-- key exactly as produced by the authenticator. The table is platform-global
-- (a user is not owned by an account) and is classified ClassPlatform.
CREATE TABLE webauthn_credentials (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  credential_id BLOB NOT NULL,
  public_key BLOB NOT NULL,
  attestation_type TEXT NOT NULL DEFAULT '',
  aaguid TEXT NOT NULL DEFAULT '',
  sign_count INTEGER NOT NULL DEFAULT 0,
  transports TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  backup_eligible INTEGER NOT NULL DEFAULT 0,
  backup_state INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  last_used_at TEXT,
  UNIQUE(credential_id)
) STRICT;
CREATE INDEX webauthn_credentials_user ON webauthn_credentials(user_id);
