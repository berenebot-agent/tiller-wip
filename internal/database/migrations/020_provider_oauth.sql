CREATE TABLE provider_oauth_tokens (
    provider_id TEXT PRIMARY KEY REFERENCES providers(id) ON DELETE CASCADE,
    access_token TEXT NOT NULL,
    refresh_token TEXT,
    token_type TEXT NOT NULL DEFAULT 'Bearer',
    expires_at TEXT,
    refresh_expires_at TEXT,
    id_token TEXT,
    scope TEXT,
    account_email TEXT,
    account_plan TEXT,
    auth_state TEXT NOT NULL DEFAULT 'connected'
        CHECK (auth_state IN ('connected','reconnect_required','unavailable')),
    last_refresh_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
