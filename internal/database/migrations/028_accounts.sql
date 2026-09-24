-- Account tenancy foundation.
--
-- Every tenant-owned row in a self-hosted/local installation belongs to the
-- single implicit account whose id is the fixed constant database.LocalAccountID
-- ('00000000-0000-0000-0000-000000000001'). Hosted accounts are created later
-- with random ids; the local account is never used in hosted mode.
CREATE TABLE accounts (
    id TEXT PRIMARY KEY,
    plan TEXT NOT NULL DEFAULT 'free',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended','deleting')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

INSERT INTO accounts(id, plan, status, created_at, updated_at)
VALUES ('00000000-0000-0000-0000-000000000001', 'free', 'active', '2026-09-19T00:00:00.000000000Z', '2026-09-19T00:00:00.000000000Z');

-- Platform-global settings are separate from account settings so that a
-- tenant-scoped read can never accidentally resolve a platform key, and so
-- account_id is NOT NULL in the tenant settings table.
CREATE TABLE platform_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

INSERT INTO platform_settings(key, value, updated_at)
SELECT key, value, updated_at FROM settings WHERE key = 'admin_credential_hash';

DELETE FROM settings WHERE key = 'admin_credential_hash';
