-- NULL means unknown; catalogue refresh supplies provider metadata or models.dev enrichment.
ALTER TABLE provider_models ADD COLUMN supports_temperature INTEGER CHECK (supports_temperature IN (0,1));
