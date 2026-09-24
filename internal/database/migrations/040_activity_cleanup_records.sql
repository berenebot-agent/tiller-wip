-- Durable tombstones coordinate Activity cleanup with core deletion. A NULL
-- client_key_id denotes an account-wide cleanup; key rows remain after the core
-- client key is deleted until Activity confirms removal.
CREATE TABLE activity_cleanup (
    account_id TEXT NOT NULL,
    client_key_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    PRIMARY KEY (account_id, client_key_id)
) STRICT;
CREATE INDEX activity_cleanup_created ON activity_cleanup(created_at);
