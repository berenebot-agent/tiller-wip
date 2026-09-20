-- Hosted legal documents and acceptance records.
--
-- Documents are operator-editable via the platform dashboard and served
-- publicly (signup must be able to link them before authentication). Only the
-- current document per slug is stored; policy-version history is managed
-- outside the product, and each acceptance records the timestamp at which the
-- user agreed.
--
-- updated_by is NULL for the migration-seeded placeholder and is set on every
-- operator save. Startup seeding replaces a row only while updated_by IS NULL,
-- so an operator edit is never overwritten by a deploy.
CREATE TABLE legal_documents (
    slug TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    updated_by TEXT
) STRICT;

CREATE TABLE legal_acceptances (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    terms_updated_at TEXT,
    privacy_updated_at TEXT,
    accepted_at TEXT NOT NULL,
    ip TEXT,
    user_agent TEXT
) STRICT;

CREATE INDEX legal_acceptances_user ON legal_acceptances(user_id, accepted_at DESC);

INSERT INTO legal_documents(slug, title, body, updated_at, updated_by) VALUES
  ('terms', 'Terms of Service', 'These Terms of Service are being finalised. By continuing to use Hosted Tiller you agree to be bound by them once published.', '2026-09-21T00:00:00.000000000Z', NULL),
  ('privacy', 'Privacy Policy', 'This Privacy Policy is being finalised. Hosted Tiller processes request content transiently and stores only account information and request metadata.', '2026-09-21T00:00:00.000000000Z', NULL);
