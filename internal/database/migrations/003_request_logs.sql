CREATE TABLE request_logs (
    id                 TEXT PRIMARY KEY,
    client_key_id      TEXT NOT NULL REFERENCES client_keys(id) ON DELETE CASCADE,
    requested_model    TEXT NOT NULL,
    resolved_provider  TEXT,
    resolved_model     TEXT,
    protocol           TEXT NOT NULL,
    streaming          INTEGER NOT NULL CHECK (streaming IN (0,1)),
    http_status        INTEGER NOT NULL,
    latency_ms         INTEGER NOT NULL,
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    provider_request_id TEXT,
    client_request_id  TEXT NOT NULL,
    error_text         TEXT,
    created_at         TEXT NOT NULL
) STRICT;
CREATE INDEX request_logs_client_created ON request_logs(client_key_id, created_at DESC);
CREATE INDEX request_logs_created ON request_logs(created_at);
