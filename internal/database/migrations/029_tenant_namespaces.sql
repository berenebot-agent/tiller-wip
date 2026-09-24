-- Account-scope the shared provider/virtual-group name registry.
--
-- `namespaces` is the one table whose key is the name itself, so it becomes the
-- single exception to the "no composite keys in SQLite" decision: PK is
-- (account_id, name) and providers/virtual_provider_groups keep a composite FK
-- to it, preserving the invariant that a provider and a virtual group cannot
-- share a name *within one account*. Names are now account-local.
CREATE TABLE namespaces_new (
    account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    name TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('real','virtual')),
    entity_id TEXT NOT NULL,
    PRIMARY KEY(account_id, name),
    UNIQUE(account_id, entity_id),
    CHECK (name = lower(name) AND length(name) BETWEEN 1 AND 63 AND name NOT GLOB '*[^a-z0-9-]*' AND substr(name,1,1) GLOB '[a-z0-9]' AND substr(name,-1,1) GLOB '[a-z0-9]'),
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE RESTRICT
) STRICT;

INSERT INTO namespaces_new(account_id, name, kind, entity_id)
SELECT '00000000-0000-0000-0000-000000000001', name, kind, entity_id FROM namespaces;

CREATE TABLE providers_new (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    name TEXT NOT NULL,
    type TEXT NOT NULL,
    base_url TEXT NOT NULL,
    credential_secret TEXT,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    protocols TEXT NOT NULL DEFAULT '["chat"]',
    last_refresh_at TEXT,
    next_refresh_at TEXT,
    last_refresh_error TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(account_id, name),
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE RESTRICT,
    FOREIGN KEY (account_id, name) REFERENCES namespaces_new(account_id, name) ON UPDATE CASCADE ON DELETE RESTRICT
) STRICT;

INSERT INTO providers_new(id, account_id, name, type, base_url, credential_secret, enabled, protocols, last_refresh_at, next_refresh_at, last_refresh_error, created_at, updated_at)
SELECT id, '00000000-0000-0000-0000-000000000001', name, type, base_url, credential_secret, enabled, protocols, last_refresh_at, next_refresh_at, last_refresh_error, created_at, updated_at FROM providers;

CREATE TABLE virtual_provider_groups_new (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    name TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(account_id, name),
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE RESTRICT,
    FOREIGN KEY (account_id, name) REFERENCES namespaces_new(account_id, name) ON UPDATE CASCADE ON DELETE RESTRICT
) STRICT;

INSERT INTO virtual_provider_groups_new(id, account_id, name, created_at, updated_at)
SELECT id, '00000000-0000-0000-0000-000000000001', name, created_at, updated_at FROM virtual_provider_groups;

DROP TABLE providers;
DROP TABLE virtual_provider_groups;
DROP TABLE namespaces;

ALTER TABLE namespaces_new RENAME TO namespaces;
ALTER TABLE providers_new RENAME TO providers;
ALTER TABLE virtual_provider_groups_new RENAME TO virtual_provider_groups;
