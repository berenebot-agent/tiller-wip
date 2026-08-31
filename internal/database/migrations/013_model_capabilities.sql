ALTER TABLE provider_models ADD COLUMN supports_tools INTEGER;
ALTER TABLE provider_models ADD COLUMN supports_vision INTEGER;
ALTER TABLE provider_models ADD COLUMN supports_reasoning INTEGER;
ALTER TABLE provider_models ADD COLUMN supports_structured_output INTEGER;
ALTER TABLE provider_models ADD COLUMN input_modalities TEXT;
ALTER TABLE provider_models ADD COLUMN output_modalities TEXT;
