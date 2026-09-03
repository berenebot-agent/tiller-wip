-- muse-spark-1.2-contributor-free and muse-spark-1.3-contributor-free are
-- served through OpenCode's Responses API (/v1/responses with `input` field,
-- `max_output_tokens`). Tiller previously forced every -free model to the
-- Chat Completions API, which the relay 500s on for these two models.
-- Correct rows created before the per-model Responses rule so an upgrade does
-- not wait for the next catalogue refresh.
UPDATE provider_models
SET native_protocol='responses'
WHERE upstream_model_id IN ('muse-spark-1.2-contributor-free', 'muse-spark-1.3-contributor-free')
  AND provider_id IN (SELECT id FROM providers WHERE type IN ('opencode-zen', 'opencode-free'));

UPDATE providers
SET protocols='["chat","responses"]'
WHERE type='opencode-free';
