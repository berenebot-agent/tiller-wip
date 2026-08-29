ALTER TABLE client_keys ADD COLUMN logging_enabled INTEGER NOT NULL DEFAULT 1 CHECK (logging_enabled IN (0,1));
ALTER TABLE client_keys ADD COLUMN retention_days INTEGER NOT NULL DEFAULT 30;
