-- Account-scope the settings key/value store.
--
-- admin_credential_hash was moved to platform_settings in migration 028, so
-- every remaining row is account-scoped and account_id can be NOT NULL.
CREATE TABLE settings_new (
    account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(account_id, key),
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE RESTRICT
) STRICT;

INSERT INTO settings_new(account_id, key, value, updated_at)
SELECT '00000000-0000-0000-0000-000000000001', key, value, updated_at FROM settings;

DROP TABLE settings;
ALTER TABLE settings_new RENAME TO settings;
