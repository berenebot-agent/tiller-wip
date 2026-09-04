-- Migration 018 introduced error_message before provider response bodies were
-- removed from the persistence path. Existing values may contain arbitrary
-- provider text, so clear them while retaining error_text/failure_class.
UPDATE request_logs SET error_message=NULL;
UPDATE request_attempts SET error_message=NULL;
