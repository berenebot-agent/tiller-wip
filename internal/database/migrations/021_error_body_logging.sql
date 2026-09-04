INSERT INTO settings(key, value, updated_at) VALUES('log_error_bodies', '0', '2026-09-04T00:00:00.000000000Z');

ALTER TABLE request_logs ADD COLUMN request_body TEXT;
ALTER TABLE request_logs ADD COLUMN request_body_truncated INTEGER NOT NULL DEFAULT 0 CHECK (request_body_truncated IN (0,1));
ALTER TABLE request_logs ADD COLUMN error_body TEXT;
ALTER TABLE request_logs ADD COLUMN error_body_truncated INTEGER NOT NULL DEFAULT 0 CHECK (error_body_truncated IN (0,1));
ALTER TABLE request_attempts ADD COLUMN error_body TEXT;
ALTER TABLE request_attempts ADD COLUMN error_body_truncated INTEGER NOT NULL DEFAULT 0 CHECK (error_body_truncated IN (0,1));
