-- Account-scope provider catalogue rows and provider OAuth tokens.
--
-- account_id is added even though it is derivable via provider_id: the roadmap
-- requires it for row-level security, tenant indexes, and to make an unscoped
-- query detectable.
ALTER TABLE provider_models ADD COLUMN account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE provider_oauth_tokens ADD COLUMN account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';

CREATE INDEX provider_models_account ON provider_models(account_id, provider_id, available, upstream_model_id);
CREATE INDEX provider_oauth_tokens_account ON provider_oauth_tokens(account_id, provider_id);
