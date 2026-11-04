-- A verification token minted by signup carries the password hash chosen in that
-- same request. Verification applies it, so a pending account's password is
-- only ever activated by the link that went with it. NULL means the token was
-- minted without a password (resend); verifying it clears any stored password and
-- the user sets one through the password-reset email.
ALTER TABLE email_verification_tokens ADD COLUMN password_hash TEXT;
