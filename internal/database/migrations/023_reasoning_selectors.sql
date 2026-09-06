-- Normalized reasoning-selector capabilities.
-- reasoning_capabilities stores JSON (NULL when unknown) describing a model's
-- advertised reasoning selector mechanisms (effort levels, toggle, budget).
-- Existing rows remain NULL until the next catalogue refresh repopulates them.
ALTER TABLE provider_models ADD COLUMN reasoning_capabilities TEXT;
