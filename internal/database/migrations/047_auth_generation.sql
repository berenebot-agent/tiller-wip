-- Authentication/recovery generation. Bumped on every sensitive credential
-- change (password change/reset, email-change confirm, revoke-all) so one-time
-- recovery tokens issued before the change can no longer be consumed, and so a
-- login that authenticated against an older generation cannot mint a session
-- after the change commits. Existing rows and any currently-unused tokens
-- default to 0 and therefore still match until the first sensitive change.
ALTER TABLE users ADD COLUMN auth_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE password_reset_tokens ADD COLUMN auth_generation INTEGER NOT NULL DEFAULT 0;
