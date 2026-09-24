-- Add the owner link and the hosted `pending` lifecycle state to accounts.
--
-- V1 is one-user-per-account: owner_user_id is unique per account, but NULL for
-- the self-hosted local account (which has no user). `pending` is the state
-- between signup and email verification; `active` remains the default for the
-- migrated local account. SQLite cannot alter a CHECK constraint in place, so
-- the table is rebuilt with the canonical create/copy/drop/rename pattern.
CREATE TABLE accounts_new (
  id TEXT PRIMARY KEY,
  plan TEXT NOT NULL DEFAULT 'free',
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('pending','active','suspended','deleting')),
  owner_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
) STRICT;

INSERT INTO accounts_new(id, plan, status, owner_user_id, created_at, updated_at)
SELECT id, plan, status, NULL, created_at, updated_at FROM accounts;

DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;

CREATE UNIQUE INDEX accounts_owner_user ON accounts(owner_user_id) WHERE owner_user_id IS NOT NULL;
