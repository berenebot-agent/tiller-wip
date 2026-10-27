-- Catalogue templates: saved, account-scoped model-permission sets that
-- catalogue client keys can link to.
--
-- A template has the same shape as a client's permission matrix (per-model
-- enables plus per-group "new models default" feeders). Linking is
-- materialised: a linked client's own client_model_permissions and
-- client_group_defaults rows are rewritten to mirror the template on every
-- template save, so routing keeps reading only the client rows.
--
-- client_keys.template_id is NULL for Custom (the pre-existing behaviour), so
-- the upgrade changes nothing for existing keys.
CREATE TABLE catalogue_templates (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(account_id, name)
) STRICT;

CREATE TABLE catalogue_template_group_defaults (
    template_id TEXT NOT NULL REFERENCES catalogue_templates(id) ON DELETE CASCADE,
    group_kind TEXT NOT NULL CHECK (group_kind IN ('real','virtual')),
    group_id TEXT NOT NULL,
    new_models_enabled INTEGER NOT NULL DEFAULT 0 CHECK (new_models_enabled IN (0,1)),
    updated_at TEXT NOT NULL,
    account_id TEXT NOT NULL,
    PRIMARY KEY(template_id, group_kind, group_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE catalogue_template_model_permissions (
    template_id TEXT NOT NULL REFERENCES catalogue_templates(id) ON DELETE CASCADE,
    model_kind TEXT NOT NULL CHECK (model_kind IN ('real','virtual')),
    model_id TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0,1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    account_id TEXT NOT NULL,
    PRIMARY KEY(template_id, model_kind, model_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX catalogue_template_permissions_model ON catalogue_template_model_permissions(model_kind, model_id);

ALTER TABLE client_keys ADD COLUMN template_id TEXT REFERENCES catalogue_templates(id) ON DELETE SET NULL;
CREATE INDEX client_keys_template ON client_keys(template_id);
