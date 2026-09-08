ALTER TABLE request_logs ADD COLUMN error_message TEXT;
ALTER TABLE request_attempts ADD COLUMN error_message TEXT;
