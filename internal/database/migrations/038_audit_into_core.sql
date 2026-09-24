-- Fold the security audit tables into the core database.
--
-- audit.db was never released; a separate file only complicated backup,
-- restore, and handle lifecycle. Audit is low-volume durable control-plane
-- history, so it now lives in router.db. It stays logically separate: account
-- audit reads remain account-scoped, platform audit is never exposed through a
-- customer API, and audit_retention_days keeps its independent setting.
--
-- account_audit_events.account_id is historical attribution, NOT a foreign key
-- to accounts(id): retained security history must survive account deletion. Do
-- not add an ON DELETE CASCADE (or any) FK here.
CREATE TABLE account_audit_events (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL,
    event TEXT NOT NULL,
    actor_type TEXT NOT NULL,
    actor_id TEXT,
    target_type TEXT,
    target_id TEXT,
    outcome TEXT NOT NULL DEFAULT 'success' CHECK (outcome IN ('success','failure')),
    metadata TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
) STRICT;
CREATE INDEX account_audit_events_account_created ON account_audit_events(account_id, created_at DESC);
CREATE INDEX account_audit_events_created ON account_audit_events(created_at);

CREATE TABLE platform_audit_events (
    id TEXT PRIMARY KEY,
    event TEXT NOT NULL,
    actor_type TEXT NOT NULL DEFAULT 'platform',
    actor_id TEXT,
    target_type TEXT,
    target_id TEXT,
    outcome TEXT NOT NULL DEFAULT 'success' CHECK (outcome IN ('success','failure')),
    metadata TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
) STRICT;
CREATE INDEX platform_audit_events_created ON platform_audit_events(created_at);

CREATE TABLE audit_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

INSERT INTO audit_meta(key,value,updated_at) VALUES('audit_retention_days','365',strftime('%Y-%m-%dT%H:%M:%fZ','now'));
