CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
INSERT INTO settings(key, value, updated_at) VALUES('default_logging_enabled', '1', '2026-08-29T00:00:00.000000000Z');
INSERT INTO settings(key, value, updated_at) VALUES('default_retention_days', '30', '2026-08-29T00:00:00.000000000Z');
