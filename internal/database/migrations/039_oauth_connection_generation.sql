ALTER TABLE provider_oauth_tokens ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;

CREATE TABLE oauth_connection_generations (
    account_id TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    generation INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, provider_id),
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
) STRICT;
