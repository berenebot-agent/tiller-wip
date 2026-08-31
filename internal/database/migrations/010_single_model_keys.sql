ALTER TABLE client_keys ADD COLUMN key_type TEXT NOT NULL DEFAULT 'catalogue'
    CHECK (key_type IN ('catalogue','single'));

CREATE TABLE client_single_bindings (
    client_key_id TEXT PRIMARY KEY REFERENCES client_keys(id) ON DELETE CASCADE,
    exposed_model_name TEXT NOT NULL DEFAULT 'main',
    real_model_id TEXT REFERENCES provider_models(id) ON DELETE RESTRICT,
    virtual_model_id TEXT REFERENCES virtual_models(id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (
        length(exposed_model_name) BETWEEN 1 AND 255
        AND exposed_model_name NOT GLOB '*[^A-Za-z0-9._~/-]*'
        AND substr(exposed_model_name,1,1) != '/'
        AND substr(exposed_model_name,-1,1) != '/'
        AND instr(exposed_model_name, '//') = 0
    ),
    CHECK ((real_model_id IS NOT NULL) != (virtual_model_id IS NOT NULL))
) STRICT;
CREATE INDEX client_single_bindings_real ON client_single_bindings(real_model_id);
CREATE INDEX client_single_bindings_virtual ON client_single_bindings(virtual_model_id);

ALTER TABLE request_logs ADD COLUMN exposed_model TEXT;
ALTER TABLE request_logs ADD COLUMN route_kind TEXT CHECK (route_kind IN ('real','virtual'));
ALTER TABLE request_logs ADD COLUMN route_model_id TEXT;
ALTER TABLE request_logs ADD COLUMN route_model TEXT;
