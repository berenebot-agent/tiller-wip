-- Durable monthly request counters.
--
-- The monthly request allowance cannot be derived from Activity: request_logs
-- lives in the disposable activity.db and free-plan Activity retention (7 days)
-- is shorter than the monthly window, so a derived count would silently reset
-- after a prune. The counter therefore lives in the core database (backed up,
-- not pruned by Activity retention).
--
-- period is the UTC 'YYYY-MM' key, so rollover needs no reset job: a new month
-- is simply a new row at zero. The row is account-scoped and every read/write
-- goes through internal/store.
CREATE TABLE usage_counters (
    account_id TEXT NOT NULL,
    period TEXT NOT NULL,
    requests INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (account_id, period),
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
) STRICT;

CREATE INDEX usage_counters_account ON usage_counters(account_id, period);
