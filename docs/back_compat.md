# Backward-compatibility index

This file is the single reference for every backward-compatibility path in the
codebase. It exists so a reviewer (or an agent editing the code) can check one
list instead of grepping for `legacy`/`compat`, and so nobody adds *new*
historical-data handling to a hot path by accident.

**Rule:** historical-data compatibility belongs in the **upgrade / migration
layer** — one-time, marker-guarded, at boot or in a versioned migration step.
It must **not** be interleaved into request handling, query predicates, or
`SELECT`/`INSERT` paths that new code is constantly edited against. If you find
yourself writing `OR (some_old_column IS NULL AND some_old_status = '…')` inside
a live query, stop: add an upgrade step that normalises the data, then write the
query against the normalised shape.

## Categories

### 1. Schema migrations (isolated, additive, append-only)
- `internal/database/migrations/*.sql` — embedded via `//go:embed` and applied
  once each via the `schema_migrations` ledger (`internal/database/database.go`,
  `DB.Migrate`). New schema changes are always a new numbered file.
- `internal/database/activity.go` `migrateActivity` — one-time move of the
  Activity tables out of the central DB into `activity.db`.
- `internal/database/database.go` `snapshotBeforeTenancy` / `installationID` —
  one-time pre-tenancy rollback snapshot.
- `internal/database/activity.go` `activityUpgrades` / `activityDataUpgrades` —
  additive, versioned `activity.db` upgrades keyed on
  `activity_schema_migrations`.
- `internal/database/activity.go` `upgradeActivityData` step
  `003_legacy_route_unresolved` — reclassifies historical `route_status='legacy'`
  rows to `'unresolved'` so the store's attribution predicates need no legacy
  disjunct (see §4).

### 2. By-design format versioning (not debt — do not remove)
- `internal/crypto/cipher.go` — `enc:v1:` storage prefix and the ordered key set
  (first key encrypts, previous keys decrypt for rotation). `docs/back_compat`
  reviewers: this is the *current* format, not a legacy shim.
- `internal/crypto/cipher.go` `Decrypt` plaintext passthrough — **PERMANENT
  COMPAT.** A stored value without the `enc:v1:` prefix is returned unchanged.
  Retained because removing it could break reads of a partially-migrated or
  restored-from-backup database. Rationale is in the function doc comment.

### 3. Entropy tiering / hashing (mostly permanent, see note)
- `internal/auth/secret_hasher.go` `VerifyEncoded` `$argon2id$` branch —
  **PERMANENT COMPAT.** Legacy argon2id *client keys* cannot be pre-migrated to
  bcrypt (hash-only, plaintext gone); they verify here and lazily upgrade. The
  `$2*$` bcrypt branch is the current format. Human admin credentials stay
  argon2id by design (`Argon2Hasher`).
- `internal/auth/keys.go` `NeedsRehash` call sites — lazy upgrade of legacy
  client-key hashes on successful use. Stays while §3's argon2id branch stays.
- Sessions are **not** argon2id anymore: the legacy ones are revoked at boot
  (see §4), so `keys.go` session rehash paths are dead-compat for sessions.

### 4. One-time upgrade markers / guards (permanent gates, not recurring compat)
- `platform_settings` keys: `hosted_bootstrap_complete`,
  `local_operator_user_id`, `admin_credential_hash`, `admin_username`.
- `accounts.owner_user_id IS NULL` condition in
  `internal/identity/local_operator.go` (`EnsureLocalOperator`) — the one-shot
  "claim the local account" guard. Stays so an install predating hosted identity
  converts exactly once.
- `internal/identity/identity.go` (`BootstrapHostedCustomer`) has **two**
  one-shot subjects behind the `hosted_bootstrap_complete` marker: (1) a
  pre-tenancy account with `owner_user_id IS NULL` is claimed by inserting a
  user (legacy path), and (2) a unified local operator row
  (`local_operator_user_id`) is **converted in place**, preserving `users.id`
  so passkeys survive. The second branch is not a compat shim in a hot path —
  it is the marker-guarded upgrade step for the local→hosted switch.
- `internal/identity/local_operator.go` (`SyncLocalOperatorCredentials`) — the
  boot-time reconciliation of the local operator row with the environment
  credential. It is a **ONE-TIME UPGRADE GUARD** for installs whose operator row
  predates a username change, not recurring compat in a request path: it is a
  no-op when nothing changed and it bumps `auth_generation` only on a real
  change.
- The route rows' `route_status='legacy'` value: **no longer produced** by new
  code (logging now defaults an unclassified row to `'unresolved'`), and
  reclassified at boot by `003_legacy_route_unresolved`. The `activity.db`
  schema `CHECK` remains permissive (still accepts `'legacy'`) on purpose so the
  rewrite needs no table rebuild.

### 5. Config deprecation (deferred; still present)
- `internal/config/config.go` `resolveLegacyCredentials` — accepts the old
  `TILLER_ADMIN_USERNAME` / `TILLER_ADMIN_PASSWORD` names with a startup notice.
  Considered low-traffic; removal is a separate, deliberate change.

### 6. Known historical-data compat already removed
- The store's virtual/real attribution predicates
  (`internal/store/activity.go` `virtualAttribution`/`realAttribution`,
  `internal/store/usage.go` `virtualAttributionFilter` and the per-target usage
  query) no longer carry the `(route_kind IS NULL AND route_status='legacy')`
  disjunct. Historical rows are normalised at boot instead (§1).

## Deleting a compat path
Before removing anything in §2–§5, confirm:
1. No deployed database can still contain the old shape (or the upgrade that
   normalises it has shipped and run); and
2. Any restored-from-backup path still reads; and
3. For §3, that no un-rehashable secret still relies on the fallback.
