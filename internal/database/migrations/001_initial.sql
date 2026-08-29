CREATE TABLE namespaces (
    name TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('real','virtual')),
    entity_id TEXT NOT NULL UNIQUE,
    CHECK (name = lower(name) AND length(name) BETWEEN 1 AND 63 AND name NOT GLOB '*[^a-z0-9-]*' AND substr(name,1,1) GLOB '[a-z0-9]' AND substr(name,-1,1) GLOB '[a-z0-9]')
) STRICT;

CREATE TABLE providers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    type TEXT NOT NULL,
    base_url TEXT NOT NULL,
    credential_secret TEXT,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    protocols TEXT NOT NULL DEFAULT '["chat"]',
    last_refresh_at TEXT,
    next_refresh_at TEXT,
    last_refresh_error TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (name) REFERENCES namespaces(name) ON UPDATE CASCADE ON DELETE RESTRICT
) STRICT;

CREATE TABLE provider_models (
    id TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
    upstream_model_id TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    available INTEGER NOT NULL DEFAULT 1 CHECK (available IN (0,1)),
    first_seen_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(provider_id, upstream_model_id)
) STRICT;
CREATE INDEX provider_models_provider_available ON provider_models(provider_id, available, upstream_model_id);

CREATE TABLE virtual_provider_groups (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (name) REFERENCES namespaces(name) ON UPDATE CASCADE ON DELETE RESTRICT
) STRICT;

CREATE TABLE virtual_models (
    id TEXT PRIMARY KEY,
    virtual_group_id TEXT NOT NULL REFERENCES virtual_provider_groups(id) ON DELETE RESTRICT,
    name TEXT NOT NULL,
    target_provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
    target_provider_model_id TEXT NOT NULL REFERENCES provider_models(id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (length(name) BETWEEN 1 AND 255 AND name NOT GLOB '*[^A-Za-z0-9._~/-]*' AND substr(name,1,1) != '/' AND substr(name,-1,1) != '/' AND instr(name, '//') = 0),
    UNIQUE(virtual_group_id, name)
) STRICT;

CREATE TABLE client_keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    selector TEXT NOT NULL UNIQUE,
    secret_hash TEXT NOT NULL,
    secret_fingerprint TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    created_at TEXT NOT NULL,
    rotated_at TEXT,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE client_group_defaults (
    client_key_id TEXT NOT NULL REFERENCES client_keys(id) ON DELETE CASCADE,
    group_kind TEXT NOT NULL CHECK (group_kind IN ('real','virtual')),
    group_id TEXT NOT NULL,
    new_models_enabled INTEGER NOT NULL DEFAULT 0 CHECK (new_models_enabled IN (0,1)),
    updated_at TEXT NOT NULL,
    PRIMARY KEY(client_key_id, group_kind, group_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE client_model_permissions (
    client_key_id TEXT NOT NULL REFERENCES client_keys(id) ON DELETE CASCADE,
    model_kind TEXT NOT NULL CHECK (model_kind IN ('real','virtual')),
    model_id TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0,1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(client_key_id, model_kind, model_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX permissions_model ON client_model_permissions(model_kind, model_id, enabled);
