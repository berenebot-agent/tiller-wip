# Backup and Restore Runbook

**Status:** Hosted-on-SQLite operational readiness

This runbook covers the central-database snapshot, off-host copying, and the
restore procedure. It is the operator-facing half of the reliability gate.

## What is backed up

The core database (`<data dir>/tiller-router.db`): accounts, hosted users and
sessions, tokens, platform settings, providers, provider models/tokens, virtual
models/groups/targets, client keys and permissions, namespaces, settings, and
the security audit history (`account_audit_events`, `platform_audit_events`,
and the `audit_retention_days` setting in `audit_meta`). Audit lives in the core
database because it is low-volume durable control-plane history; account audit
rows carry `account_id` as historical attribution with no foreign key, so they
survive account deletion.

**Activity is not backed up.** `request_logs` and `request_attempts` live in a
separate SQLite file, `<data dir>/activity.db`. It is disposable telemetry with
its own retention windows, so a restore brings the service back without
historical Activity. It is account-scoped (`account_id` on both tables) and
shared across accounts. If `activity.db` is lost, the service still starts and
serves; Activity history is simply empty (and the admin UI reports Activity as
unavailable while it cannot be opened). An operator who wants Activity history
can back up `activity.db` separately (for example with `VACUUM INTO` while the
service runs), but it is not part of the core restore contract.

## Master key (back up separately)

Recoverable provider credentials, OAuth tokens, and the notification auth header
are encrypted at rest (AES-256-GCM). The master key lives **outside** the
database:

- `TILLER_MASTER_KEY_FILE` (highest precedence) — e.g. a Docker secret;
- `TILLER_MASTER_KEY` (base64 of 32 random bytes);
- otherwise a generated `<data dir>/master.key` (0600).

Snapshot backups of `tiller-router.db` contain only ciphertext. **Without the master
key, encrypted credentials are unrecoverable**, so back the key up separately
from `./data` and in a different failure domain. If you rely on the generated
`master.key`, a full `./data` archive contains both the ciphertext and the key
and must be protected as a secret. To rotate:
`tiller-router rotate-master-key` with the service stopped, supplying the new
key via `TILLER_MASTER_KEY_NEW` / `TILLER_MASTER_KEY_NEW_FILE`; update any
env/secret source that shadows the generated `master.key`, then restart.

### Pre-rotation key sidecar (`master.key.previous`)

When the active key is the generated `<data dir>/master.key`, `rotate-master-key`
first preserves the old key as `<data dir>/master.key.previous` (0600, atomic
write) **before** it writes the new `master.key` and re-encrypts the stored
ciphertext. The sidecar is:

- **kept by default after a successful rotation** — old snapshots and any other
  copy of the pre-rotation ciphertext remain decryptable, and the operator can
  remove it deliberately once it is no longer needed;
- **part of a full `./data` archive and must be backed up** like `master.key`
  itself, since it is key material.

**Recovering from an interrupted rotation.** Rotation is crash-safe: the old key
is durable in the sidecar before the active key file is replaced. If the process
is killed after `master.key` is overwritten but before re-encryption commits, the
database is still on the old key. Copy the sidecar back over the active key and
restart:

```bash
cp ./data/master.key.previous ./data/master.key   # restore the pre-rotation key
```

The service logs a warning at startup whenever `master.key.previous` exists,
naming the sidecar and the restore path (it never logs key material). After the
recovery restart, the startup credential-migration pass verifies that existing
ciphertext decrypts before serving credentials.

## Schedule and retention

| Setting | Default | Meaning |
|---|---|---|
| `TILLER_BACKUP_INTERVAL` | `6h` | Maintenance-pass cadence: snapshot, verify, prune, then compact. `0` disables the whole pass, including compaction. |
| `TILLER_BACKUP_RETENTION` | `168h` (1 week) | Snapshots older than this are pruned locally. |
| `TILLER_BACKUP_DIR` | `<data dir>/backups` | Snapshot directory. |

The global backup export is a local-mode administrator operation. Hosted
customer sessions cannot download the installation database, and no hosted
customer export substitutes for this account boundary. Activity cleanup
reconciliation and security-audit pruning run on independent maintenance loops;
setting `TILLER_BACKUP_INTERVAL=0` disables snapshots and database compaction,
but does not disable those cleanup and retention operations.

Each scheduled snapshot is written with SQLite `VACUUM INTO`, then verified
(`PRAGMA integrity_check` + `PRAGMA foreign_key_check`) before the older
snapshots are pruned. After the snapshot, both databases are compacted in place
(SQLite `VACUUM`): the core database reclaims churn from sessions and audit, and
`activity.db` reclaims the pages freed by the hourly request-log prune. A failed
backup is logged but does not skip the vacuum. Core snapshots are named
`tiller-router-<UTC timestamp>.db` and are `0600`. A single one-time rollback
snapshot, `pre-saas-migration-<install id>-<UTC timestamp>.db`, is written to
the backup directory before the account-tenancy migration (028) upgrades an
existing pre-SaaS database. The install id (stored in SQLite's reserved
`application_id` header field, which travels with the database) binds the
snapshot to this installation, so a leftover snapshot for a different database
sharing the backup directory is never mistaken for this installation's rollback
point. The name is deliberately outside the pruning prefix so ordinary short
retention never removes it.

## RPO / RTO

- **RPO (recovery point objective): 6 hours** with defaults — the maximum
  control-plane data loss if the host is destroyed between snapshots. Lower it
  by shortening `TILLER_BACKUP_INTERVAL`. (Activity RPO is effectively N/A; it
  is not part of the recovery contract.)
- **RTO (recovery time objective): ~15 minutes** for a single-node deployment —
  provision the host, restore the snapshot (~1–2 min for a beta-scale DB), start
  the Compose stack, verify health. Actual time depends on host provisioning and
  image pull, not the database.

## Off-host copy (operator responsibility)

The container only writes snapshots to the bind-mounted data directory. Copying
them off-host is done on the host, outside the container. Any of these is fine:

```sh
# rsync to another host
rsync -a /opt/tiller-router/data/backups/ backup-host:/srv/tiller-backups/

# restic repository
restic -r s3:... backup /opt/tiller-router/data/backups

# rclone
rclone sync /opt/tiller-router/data/backups remote:tiller-backups
```

Run the copy on a host cron/systemd timer at least as often as the snapshot
cadence. Verify that the off-host destination actually grows and that at least
one copy can be restored (see below) before relying on it. Aim for copies in a
different failure domain from the router host.

## Restore procedure

1. Provision a host with Docker and Docker Compose.
2. Create the data directory and place the snapshot as the core database:
   ```sh
   mkdir -p /opt/tiller-router/data
    cp /srv/tiller-backups/tiller-router-<ts>.db \
       /opt/tiller-router/data/tiller-router.db
    ```
    The core snapshot includes the audit history, so no separate audit file is
    needed. `activity.db` is not part of the backup and is created
    automatically; the service starts with empty Activity if it is absent.
3. Start the stack:
   ```sh
   cd /opt/tiller-router && docker compose up -d
   ```
4. Verify:
   ```sh
   curl -fsS http://127.0.0.1:8080/health/ready
   # log in to the admin UI and confirm providers/models/client keys are present
   ```

## Verifying a snapshot before relying on it

`tiller-router` verifies every scheduled snapshot automatically. To verify one
manually, open it read-only and run the same checks:

```sh
# On a host with sqlite3, or via the router image:
sqlite3 /path/to/tiller-router-<ts>.db 'PRAGMA integrity_check; PRAGMA foreign_key_check;'
```

The `TestBackupExcludesActivityAndRestoresCore` unit test proves the core
snapshot restores the control plane (including `account_audit_events`) and
contains no Activity tables.

## Rolling back the account-tenancy migration

The account-tenancy migration (028) is forward-only; there are no reverse
migrations. Before it runs on an existing pre-SaaS database, Tiller writes a
verified rollback snapshot to
`<backup dir>/pre-saas-migration-<install id>-<UTC timestamp>.db` and aborts the
upgrade if the snapshot cannot be created or verified. To downgrade to the
previous Tiller release:

1. Stop Tiller.
2. Restore this installation's pre-SaaS snapshot as `router.db` (use the
   snapshot whose `<install id>` matches this database):
   ```sh
   cp /opt/tiller-router/data/backups/pre-saas-migration-<install-id>-<ts>.db \
      /opt/tiller-router/data/tiller-router.db
   ```
3. Run the **previous** Tiller image/version.
4. Start Tiller.

Do **not** run an older image directly against the newly migrated database — it
does not understand the account-tenancy schema and will misbehave.

## Failure modes

- **Snapshot fails**: logged as `scheduled backup failed`; the next interval
  retries. Repeated failures mean the disk is full or the data dir is not
  writable.
- **Verification fails**: the snapshot is left on disk for diagnosis but should
  not be trusted; investigate disk/hardware and take a fresh snapshot.
- **`activity.db` lost**: service starts normally with empty Activity and the
  admin UI reports Activity as unavailable; no restore action required unless
  Activity history matters.
- **Master key missing or wrong**: the service starts in a `locked` state; the
  admin UI reports it on the Security card and credential-bearing providers are
  unavailable. Restore the correct `master.key` (or env/secret) and restart. Do
  not delete the database to "fix" this — that discards the encrypted
  credentials.
