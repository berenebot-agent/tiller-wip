-- Per-client detailed error body logging.
--
-- log_error_bodies is nullable on purpose: NULL means "inherit", so the client
-- follows the account-level `log_error_bodies` setting exactly as it did
-- before this column existed. 1 means "always" (capture failed request and
-- provider error bodies regardless of the account setting), 0 means "never".
-- Existing rows land on NULL, so the upgrade changes no behaviour.
ALTER TABLE client_keys ADD COLUMN log_error_bodies INTEGER CHECK (log_error_bodies IS NULL OR log_error_bodies IN (0,1));
