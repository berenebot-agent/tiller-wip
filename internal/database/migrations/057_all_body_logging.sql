ALTER TABLE client_keys ADD COLUMN log_all_bodies INTEGER CHECK (log_all_bodies IS NULL OR log_all_bodies IN (0,1));
