-- opencode-free's free tier is served through the same OpenAI-compatible
-- endpoints as the paid Zen tier, but only the keyless (free) models are
-- surfaced. Correct rows created before the per-model protocol rule so an
-- upgrade does not wait for the next catalogue refresh.
UPDATE provider_models
SET native_protocol='chat'
WHERE upstream_model_id GLOB '*-free'
  AND upstream_model_id NOT IN ('muse-spark-1.2-contributor-free', 'muse-spark-1.3-contributor-free')
  AND provider_id IN (SELECT id FROM providers WHERE type IN ('opencode-zen', 'opencode-free'));

UPDATE provider_models
SET native_protocol='responses'
WHERE upstream_model_id IN ('muse-spark-1.2-contributor-free', 'muse-spark-1.3-contributor-free')
  AND provider_id IN (SELECT id FROM providers WHERE type IN ('opencode-zen', 'opencode-free'));

UPDATE providers
SET protocols='["chat","responses"]'
WHERE type='opencode-free';
