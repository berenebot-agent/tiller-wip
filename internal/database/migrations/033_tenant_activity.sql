-- Account-scope Activity (request logs and per-attempt rows).
--
-- Adding account_id to the high-churn tables is additive (no constraint
-- change); the account-leading index supports per-account Activity queries
-- and retention pruning.
ALTER TABLE request_logs ADD COLUMN account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE request_attempts ADD COLUMN account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';

CREATE INDEX request_logs_account_created ON request_logs(account_id, created_at DESC);
CREATE INDEX request_attempts_account ON request_attempts(account_id, request_log_id);
