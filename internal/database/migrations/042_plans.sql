-- Plan / entitlement definitions.
--
-- Entitlements are data, not code: a plan row holds the caps a hosted account
-- is subject to, and accounts.plan names one of them. -1 means unlimited in
-- every cap column. Local-mode accounts keep plan='free' internally but the
-- server never enforces limits outside hosted mode.
--
-- accounts.plan gains a foreign key to plans(name). SQLite cannot add a
-- constraint in place, so the table is rebuilt with the canonical
-- create/copy/drop/rename pattern (foreign keys are disabled for migrations and
-- re-verified with PRAGMA foreign_key_check afterwards).
CREATE TABLE plans (
    name TEXT PRIMARY KEY,
    max_providers INTEGER NOT NULL,
    max_client_keys INTEGER NOT NULL,
    max_virtual_models INTEGER NOT NULL,
    max_concurrent_streams INTEGER NOT NULL,
    activity_retention_days INTEGER NOT NULL,
    monthly_requests INTEGER NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

INSERT INTO plans(name, max_providers, max_client_keys, max_virtual_models, max_concurrent_streams, activity_retention_days, monthly_requests, updated_at)
VALUES ('free', 3, 5, 5, 5, 7, -1, '2026-09-21T00:00:00.000000000Z');

CREATE TABLE accounts_new (
    id TEXT PRIMARY KEY,
    plan TEXT NOT NULL DEFAULT 'free' REFERENCES plans(name) ON DELETE RESTRICT,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('pending','active','suspended','deleting')),
    owner_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

INSERT INTO accounts_new(id, plan, status, owner_user_id, created_at, updated_at)
SELECT id, plan, status, owner_user_id, created_at, updated_at FROM accounts;

DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;

CREATE UNIQUE INDEX accounts_owner_user ON accounts(owner_user_id) WHERE owner_user_id IS NOT NULL;
