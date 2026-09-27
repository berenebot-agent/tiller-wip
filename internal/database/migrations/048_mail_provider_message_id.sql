-- Retain provider-assigned message IDs for correlating accepted sends in the
-- provider console. This is metadata only and contains no message content.
ALTER TABLE mail_outbox ADD COLUMN provider_message_id TEXT;
