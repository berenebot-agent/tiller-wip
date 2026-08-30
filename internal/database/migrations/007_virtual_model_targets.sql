ALTER TABLE virtual_models ADD COLUMN routing_mode TEXT NOT NULL DEFAULT 'fixed' CHECK (routing_mode IN ('fixed','ordered_fallback'));

CREATE TABLE virtual_model_targets (
    id TEXT PRIMARY KEY,
    virtual_model_id TEXT NOT NULL REFERENCES virtual_models(id) ON DELETE CASCADE,
    provider_model_id TEXT NOT NULL REFERENCES provider_models(id) ON DELETE RESTRICT,
    position INTEGER NOT NULL CHECK (position >= 1),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(virtual_model_id, provider_model_id),
    UNIQUE(virtual_model_id, position)
) STRICT;
CREATE INDEX virtual_model_targets_route ON virtual_model_targets(virtual_model_id, enabled, position);

INSERT INTO virtual_model_targets(id,virtual_model_id,provider_model_id,position,enabled,created_at,updated_at)
SELECT 'vmt_' || id, id, target_provider_model_id, 1, 1, created_at, updated_at FROM virtual_models;

ALTER TABLE request_logs ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 1;
ALTER TABLE request_logs ADD COLUMN fallback_used INTEGER NOT NULL DEFAULT 0 CHECK (fallback_used IN (0,1));
ALTER TABLE request_logs ADD COLUMN fallback_reason TEXT;

CREATE TABLE request_attempts (
    id TEXT PRIMARY KEY,
    request_log_id TEXT NOT NULL REFERENCES request_logs(id) ON DELETE CASCADE,
    attempt_number INTEGER NOT NULL CHECK (attempt_number >= 1),
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('success','failed','skipped')),
    http_status INTEGER,
    failure_class TEXT,
    latency_ms INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(request_log_id, attempt_number)
) STRICT;
CREATE INDEX request_attempts_request ON request_attempts(request_log_id, attempt_number);
