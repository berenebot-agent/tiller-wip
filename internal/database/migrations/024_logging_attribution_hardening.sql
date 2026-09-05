ALTER TABLE request_logs ADD COLUMN route_status TEXT NOT NULL DEFAULT 'legacy'
    CHECK (route_status IN ('legacy','routed','unresolved'));

UPDATE request_logs SET route_status='routed' WHERE route_kind IS NOT NULL;

-- Body capture was a privacy footgun. Remove any material written by the old
-- opt-in setting before the new writer stops populating these columns.
UPDATE request_logs SET request_body=NULL,request_body_truncated=0,error_body=NULL,error_body_truncated=0;
UPDATE request_attempts SET error_body=NULL,error_body_truncated=0;
