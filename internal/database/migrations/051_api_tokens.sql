-- Management API tokens (local mode).
--
-- A management token authenticates the admin REST surface (the same endpoints
-- the SPA calls) with a bearer credential instead of the browser session
-- cookie. Secrets are high-entropy machine tokens, so they are bcrypt-hashed
-- (entropy tiering, like client keys); only the selector and fingerprint are
-- stored in the clear.
--
-- scopes is a JSON object mapping a resource area to its ceiling tier
-- (1=read, 2=write, 3=delete). A missing area means no access. Tier
-- implications are handled at check time (ceiling >= required).
CREATE TABLE api_tokens (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    selector TEXT NOT NULL UNIQUE,
    secret_hash TEXT NOT NULL,
    secret_fingerprint TEXT NOT NULL,
    scopes TEXT NOT NULL DEFAULT '{}',
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    expires_at TEXT,
    last_used_at TEXT,
    created_at TEXT NOT NULL,
    rotated_at TEXT,
    updated_at TEXT NOT NULL,
    UNIQUE(account_id, name),
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE RESTRICT
) STRICT;

CREATE INDEX api_tokens_account ON api_tokens(account_id);
