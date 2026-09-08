ALTER TABLE provider_models ADD COLUMN origin TEXT NOT NULL DEFAULT 'discovered'
    CHECK (origin IN ('discovered','manual'));
