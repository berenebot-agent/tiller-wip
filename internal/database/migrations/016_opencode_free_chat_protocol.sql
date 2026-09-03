-- OpenCode's anonymous Free tier is served through its OpenAI-compatible
-- Chat Completions endpoint. Correct rows created before the provider-wide
-- protocol rule so an upgrade does not wait for the next catalogue refresh.
UPDATE provider_models
SET native_protocol='chat'
WHERE upstream_model_id GLOB '*-free'
  AND provider_id IN (SELECT id FROM providers WHERE type IN ('opencode-zen', 'opencode-free'));

UPDATE providers
SET protocols='["chat"]'
WHERE type='opencode-free';
