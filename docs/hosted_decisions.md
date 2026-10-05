# Hosted Tiller — Decision Record

**Status:** Living document
**Companion:** `sass_tech.md` (technical roadmap), `saas_legal.md` (legal roadmap)
**Last updated:** 2026-09-20

This file records the architectural decisions taken while implementing the
hosted/multi-tenant roadmap. It exists so a later session can pick up the next
stage without re-litigating settled questions. Anything here overrides the
wording of `sass_tech.md` where the two disagree.

> The hosted work is re-sequenced as **Stages A–E** (`hosted_status.md`); older
> "Phase 2/3/4/5" references below map to Stage E / B / C / D respectively.

---

## 1. Phase 0 + 1 scope

- This pass implements **SAAS-001..008 only** (deployment-independent account
  tenancy on the existing SQLite application, plus the cross-account test
  harness). The later phases were re-sequenced into Stages A–E (see
  `hosted_status.md`); Phase 2/3/4/5 below map to Stage E/B/C/D.
- The tenancy work is deliberately **deployment-mode agnostic**: it makes every
  tenant-owned row account-scoped while the single self-hosted installation
  runs with exactly one implicit account.

## 2. `TILLER_MODE` deferred to Stage B (formerly Phase 3)

`sass_tech.md` §5/§39 lists `TILLER_MODE` as a Phase 0 deliverable. **Decision:
deferred to Stage B (formerly Phase 3).** In Phase 0/1 there is no hosted user,
no hosted persistence, and no hosted-only security control, so a mode switch
would gate nothing and risk a false sense of separation. Tenancy is built
unconditionally; the mode switch is introduced alongside hosted users, when
behaviour first diverges.

Consequence: the Phase 0 "hosted feature flags disabled by default" deliverable
is also deferred with it.

## 3. Persistence posture — SQLite-first, PostgreSQL deferred (revised 2026-09-19)

**Decision (human, 2026-09-19):** both self-hosted and hosted Tiller run on
**SQLite**. PostgreSQL is **deferred behind explicit trigger metrics** and is
not part of the current readiness sequence. This supersedes the earlier
"PostgreSQL is the hosted database and is built in Phase 2" wording.

Rationale:

- Phase 1 already made every tenant-owned row account-scoped and put all
  tenant-table SQL behind the `internal/store` boundary. Multi-tenant isolation
  is enforced in application queries and proven by the two-account HTTP resource
  matrix; it does not depend on the storage engine.
- SQLite in WAL mode is not the beta bottleneck. Each inference request is one
  short, best-effort Activity write transaction plus a couple of read
  transactions, and inference is upstream-latency-bound. At free-beta scale the
  binding constraints are upstream provider spend/rate limits and single-node
  availability, not SQLite write throughput.
- Supporting two engines has a real, ongoing cost: a dialect/rebind layer,
  duplicate migration sets, schema-parity tests, dual-backend CI, a
  least-privileged role split, and a store-wide refactor to wrap every tenant
  operation in a scoped transaction for Row-Level Security. That cost is not
  justified while the single-node limits have not been reached.
- The eventual port surface is bounded and known: ~136 SQL statements across
  ~19 tables and ~493 lines of migrations. Deferral is not a runaway cost.

PostgreSQL Row-Level Security remains **defence in depth**, never a replacement
for explicit account scoping. When PostgreSQL is introduced, RLS is enabled and
forced, the database role is least-privileged and cannot bypass RLS, and tenant
context is transaction-local (`SET LOCAL app.account_id`), never pooled session
state (`sass_tech.md` §8.4).

Composite ownership foreign keys (`(account_id, id)` parent keys + child FKs,
`sass_tech.md` §6.4) stay deferred with PostgreSQL. The single existing
exception is `namespaces`, whose key is the name itself.

A tenant-scoped database transaction must never be held across provider network
I/O or client streaming (`sass_tech.md` §8.4). This invariant holds on SQLite
too and is enforced by a guard test in Stage A of the re-sequenced plan.

### 3.1 Triggers to revisit PostgreSQL

PostgreSQL work is triggered by any one of:

1. a need for more than one application replica (SQLite is one file on one host,
   so it cannot back a replicated, highly-available deployment);
2. measured, sustained SQLite write-lock contention after the Stage A write-path
   tuning (`PRAGMA synchronous=NORMAL` under WAL plus batched/async Activity
   logging);
3. a decision that managed point-in-time recovery is required for the public
   beta reliability gate and cannot be met by scheduled `VACUUM INTO` snapshots
   with off-host copies;
4. Activity query latency that snapshots/batching and retention pruning cannot
   hold within budget at real volume.

### 3.2 Revised public-beta reliability gate

For V1, the `sass_tech.md` §40 reliability item "managed Postgres" is replaced
by: automated SQLite snapshot backups with off-host copies, a tested restore,
documented RPO/RTO, service health monitoring, resource/concurrency limits, a
completed load test, and an operator runbook. The RLS and
least-privileged-role items are recorded as **not designed in V1** and move with
the deferred PostgreSQL phase.

## 4. Hosted outbound / SSRF posture (decision recorded now, built in Stage C)

- Hosted mode keeps custom provider URLs and webhooks, but only through one
  shared `hostednet.SafeTransport` restricted to validated public HTTPS
  destinations (`sass_tech.md` §13).
- The address actually dialled is revalidated at connection time; redirects are
  revalidated with the same policy and bounded. Submission-time hostname
  validation alone is insufficient.
- Local/self-hosted mode retains LAN/private provider URLs. SSRF restrictions
  are hosted-mode controls and are not applied in local mode.

### 4.1 Hosted Compose deployment (2026-09-20)

- The main Compose file remains the simple self-hosted appliance, with direct
  `TILLER_PORT` publishing and no reverse proxy or host firewall requirement.
- The separate `docker-compose.hosted.yml` override uses explicit managed
  ingress and Tiller-only egress networks and does not rely on the implicit
  default network. It removes direct host-port publishing for reverse-proxy
  use.
- A reverse proxy in another Compose project may attach through
  `TILLER_INGRESS_NETWORK` with `TILLER_INGRESS_NETWORK_EXTERNAL=true`.
- Host firewall/provider-egress rules remain optional defense in depth. They are
  not embedded through privileged containers, Docker socket access, or a host
  configuration requirement.

### 4.2 Hosted registry redirects and IPv6 special ranges (2026-09-20)

- Provider, discovery, models.dev, and OAuth requests do not follow redirects.
  Provider API-key headers and OAuth request bodies must not be carried to a
  different destination; this also preserves the existing local registry
  behaviour.
- Webhook requests may follow up to three redirects, with the hosted URL policy
  re-applied to every hop and the final dial target.
- Hosted address validation rejects IPv6 translation and transition ranges
  (`64:ff9b::/96`, `64:ff9b:1::/48`, `2001::/32`, `2002::/16`, and
  `fec0::/10`) in addition to private, reserved, link-local, and metadata
  ranges. This prevents NAT64/transition addresses from representing a blocked
  IPv4 destination.

## 5. Provider credential encryption — RESOLVED 2026-09-19

**Decision (human, 2026-09-19):** the deferral is revoked. Recoverable-secret
encryption is **always-on in both self-hosted and hosted modes** — not optional.

- Algorithm: AES-256-GCM (stdlib, no new dependency), random nonce per value,
  storage format `enc:v1:<base64 nonce>:<base64 ciphertext+tag>`, AAD binding
  account/kind/record id/field.
- Key source: `TILLER_MASTER_KEY` / `TILLER_MASTER_KEY_FILE` only (file takes
  precedence); no Vault/KMS. When neither is set, a 32-byte key is generated once
  and persisted at `<data dir>/master.key` (0600). Encrypted rows with no
  available key start the process in the `locked` state rather than minting a new
  key.
- Rotation is in scope: the versioned format is decrypt-with-previous-capable and
  `tiller-router rotate-master-key` re-encrypts all secrets, tested.
- Encrypted fields: `providers.credential_secret`, `provider_oauth_tokens`
  (`access_token`, `refresh_token`, `id_token`, `provider_data`), and the
  notification `notifications_auth_header` setting. Client API keys and the admin
  credential remain hash-only (entropy-tiered) and are not encrypted.
- Boundary: encryption/decryption lives in `internal/store` (the single tenant-SQL
  boundary), which has the account/provider context the AAD needs. `internal/crypto`
  is the cipher.

Implementation: Stage C1, branch `hosted-ready`. The design of record remains
`docs/archive/roadmap_credential_encryption.md`; where it said "optional/disabled by
default", this decision supersedes it. The earlier conflict is closed.

## 6. Phase 1 implementation decisions

## 6.1 Stage B hosted-user decisions (2026-09-20)

- `TILLER_MODE` accepts `local` (default) and `hosted`; hosted requires an
  explicit HTTPS-origin `TILLER_PUBLIC_URL`.
- Local mode keeps the existing env-admin session. Hosted customer sessions,
  hosted platform sessions, and local admin sessions are separate boundaries
  with separate cookies, CSRF checks, and revocation paths.
- **Amended 2026-10-05 (partial reversal — credential storage only).** Local
  operator credentials now live in the shared `users` table via a single local
  operator row (`identity.EnsureLocalOperator`): a synthetic
  `<username>@local.invalid` user owning `LocalAccountID`, whose password hash
  is seeded from the existing `admin_credential_hash` at boot (or written by the
  first-run setup page). **Amended 2026-10-05:** a boot-time sync
  (`identity.SyncLocalOperatorCredentials`) updates the row's synthetic
  username/email, credential hash, and auth generation together when the
  environment credential changes, so `TILLER_USERNAME`/`TILLER_PASSWORD` still
  behave as seed/override/recovery; a changed username no longer leaves the
  email bound to the old name. This row is also the subject of the local→hosted
  conversion in §9 (converted in place, preserving its `users.id`). This lets
  standalone and hosted share one credential store and lets passkeys (bound to
  `users.id`) work in both modes. The **cookie
  and session boundary is unchanged**: local mode still uses `admin_sessions`,
  `requireAdmin`, and the separate admin CSRF/revocation path. The decision table
  row "Browser session storage" therefore still holds for `admin_sessions`; only
  the "separate credential storage" half is reversed. Passkeys in local mode
  require `TILLER_PUBLIC_URL` (RP/origin); when unset, passkey routes 501 and an
  INFO line says so. Password sign-in can never be disabled locally (recovery
  path). See `docs/tiller_passkeys_plan.md`.
- `TILLER_USERNAME` / `TILLER_PASSWORD` are the local operator credential
  (formerly `TILLER_ADMIN_USERNAME` / `TILLER_ADMIN_PASSWORD`, still accepted
  with a startup deprecation warning). In local mode they are the admin login;
  in hosted mode they are the optional one-time migration input. The hosted
  platform console uses the separate environment-only
  `TILLER_PLATFORM_ADMIN_USERNAME` / `TILLER_PLATFORM_ADMIN_PASSWORD`
  credentials. See §9 for the credential split and operator-account decision.
- Signup creates one pending account per user. Verification activates it;
  password reset consumes a hash-only one-time token and revokes all customer
  sessions. Account suspension immediately blocks customer sessions and client
  keys; deletion is synchronous and terminal, and is **customer- or
  operator-triggered** (amended 2026-09-20: the Account page adds instant
  self-service deletion; see §10).
-   Signup is off by default and is controlled from the platform dashboard.
  Resend, Brevo, and generic SMTP are supported. Mail settings are
  hot-reloadable; recoverable mail secrets are encrypted at rest and never
  returned by the dashboard.
- Account and platform audit events live in the core `router.db` (folded in from
  the never-released `audit.db` on 2026-09-20). Audit stays logically separate:
  account reads are account-scoped, platform audit is never exposed through a
  customer API, and `audit_retention_days` keeps its own `audit_meta` setting.
  `account_audit_events.account_id` has no foreign key, so retained history
  survives account deletion. Audit is included in the core backup; Activity
  remains excluded.
- Hosted mode never persists detailed request/provider error bodies, regardless
  of the account settings value.

| Decision | Choice |
|---|---|
| Local account id | Fixed constant UUID `00000000-0000-0000-0000-000000000001` (`database.LocalAccountID`), inserted and backfilled in pure SQL. Hosted accounts later use random `id.New()` UUIDs. |
| `account_id` coverage | Every tenant-owned table, including derivable children (`sass_tech.md` §6.3). |
| Ownership integrity | Account-local uniqueness (`UNIQUE(account_id, name)`); composite FKs deferred to Postgres — **except `namespaces`**, which keeps PK `(account_id, name)` and a composite FK from `providers`/`virtual_provider_groups`. |
| Settings | New `platform_settings` table holds future global keys (including the operator pin `operator_user_id`); `settings` becomes account-scoped with PK `(account_id, key)`. The obsolete `admin_credential_hash` row is removed in migration 039. |
| Persistence boundary | New `internal/store` package, the single boundary for all tenant-table SQL (server, `providers.Manager`, OAuth). |
| Store scoping | Per-request scoped handle built from the verified principal (`store.Scoped(accountID)`), never an id passed from request input. |
| In-memory state | All tenant-variable in-memory keys are account-scoped, matching `AGENTS.md` invariant 6. |
| Migrations | SQLite constraint changes use the canonical rebuild pattern on a dedicated FK-off connection with `PRAGMA foreign_key_check`. |
| `accounts.owner_user_id` | Deferred to the Stage B (formerly Phase 3) migration that introduces `users`. |
| Browser session storage | `admin_sessions` stays platform-global in the local/self-host mode; becomes `user_sessions` in Stage B (formerly Phase 3). Local passkeys (2026-10-05) bridge the ceremony to an `admin_sessions` session, so this remains true even though credentials now live in `users`. |
| Client-key principal lookup | The pre-account `client_keys` lookup used by authentication lives in `store.Store.ClientKeyBySelector` (platform-level), keeping `internal/auth` inside the SQL boundary. |
| SQL boundary enforcement | New guard test `internal/database/sql_boundary_test.go` fails if tenant-table SQL (a tenant table following a SQL verb in a Go string literal) reappears outside `internal/store`/`internal/database`. |
| Response-header timeout | Applied per account via immutable registry clients (`Registry.ClientFor`), replacing the process-global `SetResponseHeaderTimeout` mutation. A base client remains for discovery/OAuth and test overrides. |
| Live SSE fan-out | Subscriber sets are keyed by account; outcome/activity deltas carry their account and fan out only to that account's subscribers. |
| Usage cache | Per-account `usageAgg`/`usageAggAt` maps; `invalidateUsageAggregates(accountID)` for tenant writes and `invalidateAllUsageAggregates()` for the platform-wide retention pruner. |
| Tests | In-package Go tests, normal `go test ./...` tier; plus the browser suite. The two-account matrix injects a non-local admin principal via the unexported `withAdminAccount` server option. |
| Delivery | Single PR, branch `hosted-ready`, commits per major step. |

## 7. Re-sequenced carry-forward notes (2026-09-19)

The hosted work now runs as stages A–D on SQLite, with PostgreSQL deferred
(§3). See `hosted_status.md` for the stage table and current status.

- **Stage A — hosted-on-SQLite operational readiness.** SQLite write-path tuning
  (`PRAGMA synchronous=NORMAL` under WAL plus batched/async Activity logging),
  scheduled `VACUUM INTO` snapshot + off-host copy, tested restore, documented
  RPO/RTO, the tenant-transaction-lifetime guard, and live-SSE/cache isolation
  tests.
- **Stage B — hosted users.** `TILLER_MODE` and hosted feature flags land here
  (moved from the original Phase 0/3), with users, password hashing
  (memory-hard KDF for low-entropy human passwords), email verification, user
  sessions, password reset, automatic one-account-per-user, and the separate
  platform-admin boundary. The email provider is a new external dependency and
  needs explicit sign-off.
- **Stage C — secret custody and outbound safety.** Split: **C1 COMPLETE
  (2026-09-19)** — credential/OAuth/webhook encryption, auto-generated key, and
  tested `rotate-master-key`; the §5 conflict is resolved and encryption is
  always-on in both modes. **C2 application controls COMPLETE (2026-09-20)** —
  shared hosted `SafeTransport`/SSRF policy is integrated for provider and
  webhook traffic. Deployment-specific egress restrictions remain outstanding
  until the hosted network is defined.
- **Stage D — hosted product shell.** Domains, onboarding/wizards,
  entitlements/quotas, usage UI, and the legal/acceptance pack.
- **Deferred — PostgreSQL + RLS.** The migration must add the composite
  ownership FKs deferred here. The `sass_tech.md` §8.5 tenant-isolation schema
  test currently runs against SQLite classification only; it gains the
  RLS/role checks only when PostgreSQL lands.
- New tenant-owned tables added in Stages B–D still require account scope, a
  `database.TableClassification` entry, and isolation-matrix coverage. Only the
  PostgreSQL RLS classification half of `AGENTS.md` invariant 11 is deferred.

## 8. Stage A implementation decisions (2026-09-19, revised 2026-09-20)

The Stage A brief said not to split the Activity file. The human driving the
change reversed that: separation, easy per-user deletion, and logging that
cannot block the control plane outweighed the FK/atomicity cost, so Activity
was initially split per account (`data/activity/<account_id>.db`).

**Revised 2026-09-20:** the per-account split is replaced by a single shared
`data/activity.db` with `account_id` on both Activity tables. The file boundary
never bought Activity-to-control-plane isolation (the separate file did that on
its own) — it only isolated SQLite write locks *between accounts*, and it
required an LRU file-handle manager, filename-as-account-id validation, and a
per-account prune fan-out. The target hosted datastore is PostgreSQL, where
per-account files have no analogue and collapse to `account_id`-scoped tables,
so building per-account SQLite scaling machinery was stranded investment.
SQLite is the zero-dependency self-host backend; PostgreSQL is the intended
**eventual** hosted backend behind the same `internal/store` boundary — it
remains deferred per §3, so V1 hosted Tiller runs this same single-file SQLite
layout.

| Decision | Choice |
|---|---|
| Activity storage | One SQLite file `data/activity.db` (not per account); central `tiller-router.db` is control-plane only. `account_id` on both tables, account-leading index. A Postgres backend later is a column-for-column move of these self-describing rows. |
| Activity schema | Same columns plus a denormalized `client_name`; `account_id` retained (defaults to the local account for the one-time move); `request_attempts -> request_logs` FK stays within the file. |
| Cross-file loss | No FK/join to `client_keys` across databases. `client_name` is denormalized; usage-by-virtual groups on the stored `route_model`. |
| Client-key delete | Best-effort eager delete of the key's Activity rows after the core delete. Retention prunes by current keys, so orphans cannot be attributed afterwards — hence eager cleanup rather than pure lazy. |
| Retention | Platform-level prune runs account-scoped DELETE against the shared Activity database; retention windows are read from the central `client_keys`. |
| Write path | `synchronous=NORMAL` under WAL removes per-commit fsync; the async writer batches per account (one transaction per account batch) and drops on a full queue. `recordLastOutcome` stays synchronous for live SSE. |
| Backups | Core-only `VACUUM INTO`, 6h interval, 1 week local retention, verified with `integrity_check` + `foreign_key_check`. `activity.db` is not part of the core backup and may be backed up separately. Off-host copy is host-side operator work; RPO=interval, RTO=restore+boot. |
| Soft-fail Activity | `activity.db` opening failure is non-fatal: the handle is left nil, Activity reads return empty, writes no-op, and Tiller still starts and routes. |
| Tx-lifetime guard | Runtime probe (`store.ActiveTenantTransactions`) plus an AST lint; the `RunTx` API is the boundary, and `resolveRoute` commits before hydration/upstream. |
| Semantic shifts | Renames no longer coalesce virtual usage; deleted virtual models retain history. Both documented in `hosted_status.md`. |

## 9. Durable platform-operator account (2026-09-20)

**Decision (human, 2026-09-20):** the deployment admin
(`TILLER_USERNAME` / `TILLER_PASSWORD`, then named
`TILLER_ADMIN_USERNAME` / `TILLER_ADMIN_PASSWORD`) is a **real, durable user
account** in both modes, not a config-derived string comparison. The env values
are the desired state applied onto one pinned `users` row.

Motivation: the earlier hosted operator identity was derived by comparing the
signed-in user's email against `TILLER_ADMIN_USERNAME`, and the credential was a
separate `admin_credential_hash` fingerprint of `username + "\x00" + password`.
Switching an install from local mode (username `tiller`) to hosted mode (username
must become an email) therefore looked like a credential change: it revoked every
session even though the secret was unchanged, and a non-email local username
could not be represented at all. The replacement deliberately keeps platform
administration and customer identity separate.

- **Platform credentials.** `TILLER_PLATFORM_ADMIN_USERNAME` and
  `TILLER_PLATFORM_ADMIN_PASSWORD` are environment-only credentials. The
  `/platform` session has no `users.id` or account authority.
- **Credential storage.** `platform_settings.admin_credential_hash` stores an
  Argon2id fingerprint of the platform username/password material. A change
  revokes all platform sessions. Local mode keeps the same setting for local
  admin-session rotation, but local credentials never become a hosted user.
- **Customer migration.** Existing local deployments may provide
  `TILLER_USERNAME` and `TILLER_PASSWORD` once in hosted mode. If valid,
  startup converts the account into a verified hosted customer and keeps it on
  `LocalAccountID` in a single transaction. **Amended 2026-10-05.** A local
  install that has already booted on the unified credential model
  (`identity.EnsureLocalOperator`) owns `LocalAccountID` through a local
  operator `users` row. In that case bootstrap **converts that same user in
  place** — preserving `users.id`, so passkeys bound to it survive the switch —
  replacing the synthetic `<username>@local.invalid` email with the supplied
  hosted email and re-hashing the credential with hosted bare-password
  semantics (the local username-bound fingerprint is not valid for hosted
  login). A pre-tenancy account with no owner is still claimed by inserting a
  user, as before. Fresh hosted installs create no customer. A durable
  `hosted_bootstrap_complete` marker makes the operation idempotent.
- **Boundaries.** Customer `/login` sessions and platform `/platform` sessions
  use separate cookies, CSRF tokens, and revocation paths. No customer payload
  contains a platform-admin flag, and no platform session grants tenant access.
- **Schema.** Migration 039 and `platform_admin_sessions.user_id` are not part
  of the release. The unreleased operator-as-user history is rewritten rather
  than supported through compatibility code.

## 10. Customer Account page + durable mail outbox (2026-09-20)

**Decision (human, 2026-09-20):** hosted customers get a self-service Account
page, and transactional mail becomes durable. Both land in Stage D.

### Account page

- Surfaced as the first tab of a tabbed Settings view, hosted-only. No new
  top-level nav item (avoids restructuring the mobile quick-bar).
- **Instant self-service deletion.** The customer re-enters their password and
  types their email; the same synchronous, terminal purge path as operator
  deletion runs immediately. A grace period was considered and rejected: it
  would have required a new cancellable `deleting` state, a background purger,
  and relaxing login for `deleting` accounts — several times the code for a
  benefit the password re-entry already largely provides. §6.1 amended.
- **Change password** verifies the current password, keeps the initiating
  session, revokes all others, and cancels any pending email change.
- **Change email** is verify-new-first with password re-authentication: a
  confirmation link goes to the new address, a warning to the current address at
  request time, the current address stays the login identity until confirmation,
  and confirmation revokes every other session (retaining the initiating one via
  a stored `keep_session_id`). An address already owned by another account
  returns the same generic response but sends no mail.
- **Log out all sessions** revokes every session including the current device.
- Audit is written to both streams: account events (`user.password_changed`,
  `user.email_change_requested`, `user.email_changed`, `user.sessions_revoked`,
  `user.account_delete_requested`) and platform events
  (`platform.user_password_changed`, `platform.user_email_changed`,
  `platform.user_sessions_revoked`, `platform.account_deleted`).

### Durable mail outbox

- New `internal/mailoutbox` package owns `mail_outbox` and its worker; identity
  flows enqueue inside the transaction that creates the one-time token
  (`Enqueue(ctx, tx, msg)`), then nudge after commit.
- **Payload security.** The raw one-time token is the only encrypted field
  (`enc:v1:`, AAD-bound to the row, existing master key) and is scrubbed on
  successful send. Recipient and parameters stay readable for operator triage.
  Plaintext payloads were rejected: a leaked core backup would otherwise yield
  account takeover via a live reset token.
- **Retry.** Attempt 1 immediate; failures 1–4 schedule +1m/+5m/+30m/+2h;
  failure 5 dead-letters (row retained, token scrubbed). `ErrNotConfigured`
  stops the pass without consuming an attempt. Dead-letter auditing is injected
  as a callback so `mailoutbox` does not depend on `internal/store`.
- The two tables (`mail_outbox`, `email_change_tokens`) stay in the **core**
  database to preserve signup/enqueue atomicity; a scheduled pass prunes
  expired/used token rows and sent/dead mail rows (30-day retention).
- Operators get a read-only platform dashboard card (queued; dead in the last
  24h). The worker and table exist but stay inert in local mode.

---

## 11. Stage D — hosted product shell decisions (2026-09-21)

**Decision (human, 2026-09-21):** Stage D targets **private alpha**, not public
beta. The published legal pack is included now (the mechanism is easier to build
before signup is public); external monitoring, provider-terms sign-off, the
`app.`/`api.` origin split, and a recorded load-test run remain public-beta
gates.

### Origins

- **Single origin for alpha.** `/v1` already authenticates with a Bearer client
  key and never accepts the human session cookie, so separate `app.`/`api.`
  origins buy edge policy and cookie non-transmission only. Deferred to public
  beta.

### Entitlements

- **Plans are data, not code.** A `plans` table holds the caps; `accounts.plan`
  references it by name. The platform dashboard edits plans, and an operator can
  move an account between plans (audited `platform.account_plan_changed`). `-1`
  is the unlimited sentinel.
- **Enforcement is hosted-only.** Local/self-host has no limits (invariant 10);
  the enforcement flag is on the `Store`/`Scope` and set only when
  `TILLER_MODE=hosted`.
- **Creation limits run inside the create transaction** (count + insert atomic;
  no TOCTOU), returning `store.LimitExceededError` → `409 limit_exceeded`.
- **Concurrent-stream and monthly limits are reserved atomically after auth
  and model resolution, before upstream work**, and return `429` with
  `Retry-After`. Concurrency is reserved under the in-flight tracker's lock
  (counting live requests, not map entries, so parallel same-route requests all
  count) and the monthly slot is reserved in the core DB in one transaction
  (check + increment), so a burst cannot all observe the same pre-burst count.
- **The monthly counter lives in the core database** (`usage_counters`, period
  key `YYYY-MM`), not in Activity: Activity retention (7 days on free) is
  shorter than the monthly window, so a derived count would reset after a prune,
  and `activity.db` is disposable. One reservation per admitted routed inference
  request (not per upstream attempt). Because the reservation runs on the request
  path before upstream, a rejected request does not increment and enforcement is
  independent of Activity logging (a client key with logging disabled cannot
  bypass the cap). The unlimited path still records display-only usage
  best-effort.
- **Retention is clamped, not rewritten**: effective = `min(configured, plan
  max)` at enforcement time, so a plan upgrade restores history.
- **Downgrades grandfather existing resources**; only new creates are blocked.
- Alpha seed: free plan with 3 providers / 5 client keys / 5 virtual models /
  5 concurrent streams / 7-day retention / **unlimited monthly requests**.

### Onboarding and acceptance

- **Verify-email auto-logs-in.** The verification link mints a session for its
  own user so the first-run wizard opens immediately; the exit gate is a
  successful BYOK request.
- **Onboarding completion is derived from Activity** (a 2xx routed request),
  with an `onboarding_dismissed` account setting for skip. No persisted state
  machine.
- **Wizard drives existing admin endpoints** (provider/client-key/route create)
  rather than new bespoke backend flows, so it cannot drift from the tested CRUD
  paths. Quickstart is a single curl example (scope decision).
- **Signup requires an explicit Terms checkbox** and records a
  `legal_acceptances` row (current document + timestamp) in the signup
  transaction. **No re-prompt on document change** — policy-version handling is
  outside the product by decision; documents are operator-editable and only the
  current text is stored.
- **Google account linking is authority-gated (2026-10-01).** A Google identity
  whose email matches an existing Tiller account is linked automatically only
  when Google is authoritative for the address: `gmail.com`/`googlemail.com`,
  or a verified Workspace account with a non-empty `hd` claim. For a
  third-party address, `email_verified` does not prove current mailbox control
  (Google documents that ownership of a third-party mailbox can change while
  the claim stays true), so the identity is not linked from the unauthenticated
  callback. Instead the visitor signs in to the existing account and Tiller
  offers the link on the spot; the link itself happens through the
  authenticated Google redirect callback. Both Google entry points (redirect
  and GSI) apply the same rule.
- **An existing-account link must not be terminal.** Linking disables password
  sign-in and recovery, so the product must not leave a user with no way back.
  The authenticated link callback issues a fresh session (linking revokes the
  initiating one), the Account page offers an explicit link action, and the
  post-login interstitial is skippable. Reassess this if an admin-side unlink
  ever becomes necessary.

### Legal documents

- **DB-stored and operator-editable** via the platform dashboard; the database is
  the sole source of truth. There is no in-tree draft legal text:
  `internal/legal/` is a slug/title registry plus a **generator** for the
  placeholder body. What the router ships is instructional scaffolding — an
  explicit `NOT PUBLISHED` banner, the publish path, and the bracketed operator
  fields each document must carry (Terms: governing-law state; Privacy: the
  subprocessor vendor list) — so a fresh deploy never serves something that
  reads like approved text. The approved drafts are maintained outside the
  repository in `legal/` for legal counsel. (Changed 2026-10-01: the embedded
  unreviewed `terms.md`/`privacy.md` drafts were removed from the source tree.)
- **Seeding is conditional and re-runs every boot.** `store.SeedLegalDoc` writes
  only a row whose `updated_by IS NULL`, so the placeholder is refreshed until an
  operator publishes and never after. That is what makes `updated_by IS NULL`
  load-bearing: it is the "never published" marker, and it is why a re-deploy
  cannot clobber approved text.
- **Rendered as plain text** (`textContent`) — no markdown renderer and no new
  dependency (AGENTS.md dependency rule). Served publicly because signup links
  them before authentication.
- **Drafted for an Australian operator with AU and/or US hosting and global
  clients** (APP 1/8, GDPR Art. 3 + Chapter V, UK GDPR), with explicit
  `[PLACEHOLDER]` tokens for entity/ABN/address/contacts and a legal-review
  banner. Not publish-ready without review.

### Data export

- **Account-scoped ZIP** (config JSON + Activity CSV + audit CSV) under
  `requireUser`, streamed in bounded chunks (invariant 6), capped, and never
  containing provider credentials or key hashes. Distinct from the admin
  whole-database backup export.

## 12. Optional consent-gated hosted analytics (2026-09-29)

- **Consent first.** Analytics is off unless the platform operator enables it,
  and even then the script is requested by the browser **only after** the
  visitor accepts the bottom consent banner. Declining is remembered; the script
  is never loaded on decline. No server-side analytics or page-view collection
  is added.
- **Operator-supplied, non-secret config.** The operator chooses a provider
  (`umami`, `plausible`, or a custom https script URL) and supplies the script
  URL and a site/domain ID. These values are public (served to every page) and
  stored in plaintext `platform_settings`; they are **not** recoverable secrets
  and are not run through the cipher.
- **CSP is the enforcement point.** The operator's script origin is added to the
  hosted `script-src`, `connect-src`, and `img-src` from an in-memory cache
  refreshed at boot and on save. Only absolute `https` URLs with no embedded
  credentials are accepted. Local mode never registers the endpoint and never
  widens the policy.
- **Umami is the primary provider**; Plausible and a generic custom URL are
  also supported. Matomo was explicitly deferred (its inline `_paq` bootstrap
  needs a CSP nonce and was not requested).
- **Legal posture.** The prior "no analytics" statements in the Privacy Policy
  and `docs/saas_legal.md` are replaced with consent-gated analytics language.
  The analytics vendor is listed as an optional subprocessor. Legal review of
  the final consent wording remains required before public launch.
