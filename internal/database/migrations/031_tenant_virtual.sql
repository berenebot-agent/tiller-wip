-- Account-scope virtual routes and their ordered-fallback targets.
ALTER TABLE virtual_models ADD COLUMN account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE virtual_model_targets ADD COLUMN account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';

CREATE INDEX virtual_models_account ON virtual_models(account_id, virtual_group_id);
CREATE INDEX virtual_model_targets_account ON virtual_model_targets(account_id, virtual_model_id, enabled, position);
