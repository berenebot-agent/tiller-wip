-- Account-scope client API keys and their permission/binding children.
--
-- client key names become account-local; the selector stays globally unique
-- (it is a 9-byte random lookup token).
CREATE TABLE client_keys_new (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    selector TEXT NOT NULL UNIQUE,
    secret_hash TEXT NOT NULL,
    secret_fingerprint TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    created_at TEXT NOT NULL,
    rotated_at TEXT,
    updated_at TEXT NOT NULL,
    logging_enabled INTEGER NOT NULL DEFAULT 1 CHECK (logging_enabled IN (0,1)),
    retention_days INTEGER NOT NULL DEFAULT 30,
    key_type TEXT NOT NULL DEFAULT 'catalogue' CHECK (key_type IN ('catalogue','single')),
    key_group TEXT NOT NULL DEFAULT 'default',
    UNIQUE(account_id, name),
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE RESTRICT
) STRICT;

INSERT INTO client_keys_new(id, account_id, name, description, selector, secret_hash, secret_fingerprint, enabled, created_at, rotated_at, updated_at, logging_enabled, retention_days, key_type, key_group)
SELECT id, '00000000-0000-0000-0000-000000000001', name, description, selector, secret_hash, secret_fingerprint, enabled, created_at, rotated_at, updated_at, logging_enabled, retention_days, key_type, key_group FROM client_keys;

DROP TABLE client_keys;
ALTER TABLE client_keys_new RENAME TO client_keys;

ALTER TABLE client_group_defaults ADD COLUMN account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE client_model_permissions ADD COLUMN account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE client_single_bindings ADD COLUMN account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
