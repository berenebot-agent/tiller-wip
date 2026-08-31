CREATE TABLE admin_sessions (
    id           TEXT PRIMARY KEY,
    token_hash   TEXT NOT NULL,
    csrf_token   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    last_used_at TEXT NOT NULL
) STRICT;
CREATE INDEX admin_sessions_expires ON admin_sessions(expires_at);
