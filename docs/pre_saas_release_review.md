# Pre-SaaS Public Release Review — Tiller Router (Hosted Free Tier)

**Scope:** final engineering review before exposing the application to arbitrary internet users on the free hosted SaaS tier. Reviewed static-code only (no tests run), sequentially, against the working tree of `repo/` on `master` (commit `ec378fb` + uncommitted bootstrap-warning changes), covering all 20 review areas plus a six-persona adversarial second pass. Deliberate design decisions verified against `AGENTS.md`, `SECURITY.md`, `docs/hosted_decisions.md`, `docs/hosted_status.md`, `docs/provider_terms_review.md`, and in-code comments; uncertain intent marked **Needs Design Confirmation**.

---

## Release verdict

> **Reading order (this file is an append-only review log).** The
> "NO-GO UNTIL BLOCKERS FIXED" verdict in the *Verdict re-check after the
> adversarial pass* section below is the **original second-pass verdict** and
> remains the historical record. It was **superseded on 2026-10-04** by the
> verdict in this section once the P0/P1 code fixes landed. The current verdict
> is the one here.

**GO WITH MINOR FIXES.** All P0 and P1 code findings are fixed in `master` (2026-10-04): TR-001 server-side `HostedDisabled` rejection, TR-002 buffer ceilings (64×8 MiB inbound / 32×16 MiB outbound), TR-003 free-plan default monthly cap (20,000/month, migration 050), TR-004 hosted 30/hour notification budget, TR-005 per-email signup limiter + 30-day pending-account GC, TR-007 live-SSE cap (8/account), TR-014 config-mutation audit events + request-log account attribution, and the two small P2s TR-008 (HSTS) and TR-010 (platform session TTL 12 h); TR-013 is documented. **Remaining before announcing (operator actions):** enable Turnstile; run the TR-011 load test. Everything else is P2 (TR-006 documented fail-open, TR-009 retention-policy decision, TR-012 scale).

Two blockers were fixed, both small and well-contained:

1. **TR-002 (HIGH, DoS):** aggregate memory amplification in the inference body path — a single authenticated free-tier account can OOM the 4 GiB host (256 concurrent body reads × 32 MiB buffers ≈ 8 GB, no plan limit applies at that stage).
2. **TR-001 (HIGH, policy enforcement):** the operator decision to disable `opencode-free` for hosted is enforced only in the frontend; the server accepts it via direct API call.

Everything else is P1/P2. The codebase is in notably good shape for a beta: tenant isolation, crypto custody, SSRF policy, auth hardening, and log hygiene are all genuinely strong (see Positive Controls). Fix the two P0s, set the free plan's monthly cap (data change), enable Turnstile, and this is a defensible public free beta.

---

### Architecture summary

Single Go binary (scratch container, root-then-privdrop by design), stdlib `net/http` ServeMux, pure-Go SQLite (`modernc`, WAL, `synchronous=NORMAL`, busy_timeout 5 s, 8 conns core / 4 activity). Control plane (`tiller-router.db`) is split from Activity (`activity.db`, best-effort, excluded from snapshots).

**Trust boundaries and auth planes:**

- **Local admin** (`tiller_admin_session`, Strict SameSite, HttpOnly, conditional Secure) — one implicit account (`LocalAccountID`).
- **Hosted user** (`__Host-tiller_session`, Strict/Secure/HttpOnly) — email+password (argon2id 64 MiB/3/4), Google (PKCE+nonce), passkeys (WebAuthn, RPID = public origin); one pending→active account per user; `auth_generation` guards stale-credential session minting.
- **Platform operator** (`__Host-tiller_platform_session`) — env-only credentials (`TILLER_PLATFORM_ADMIN_*`), separate route tree mounted only in hosted mode (server.go:578-597).
- **Client API keys** (`sk-tr-selector.secret`, bcrypt-hashed, 256-bit secrets) for `/v1/*` — never cookie-authenticated.

**Tenancy:** `accountKey` is set only by auth middleware; `store.Scope` binds every tenant query to `accountID`; hosted handlers fail closed on a missing principal (server.go:829-841); guard tests enforce the SQL boundary and table classification; in-memory state (cooldown, inflight, lastOutcome, live SSE, usage caches, OAuth flows) is account-keyed.

**Secrets:** AES-256-GCM (`enc:v1:`, AAD-bound account/kind/id/field) at the `internal/store` boundary; master key outside the DB (env/file/generated, 0600); locked state fails loud; rotation CLI with sidecar recovery. Non-recoverable secrets hash-only, entropy-tiered (argon2id human, bcrypt machine).

**Outbound (hosted):** shared `hostednet.SafeTransport` — HTTPS-only, port 443, no userinfo, dial-time IP validation of every DNS answer (rebinding-safe), blocked internal suffixes/ranges incl. NAT64; providers/discovery/OAuth never follow redirects; webhooks follow ≤3 revalidated redirects.

**Free plan (seed migration 042):** 3 providers, 5 client keys, 5 virtual models, 5 concurrent streams, 7-day Activity retention, **unlimited monthly requests (-1)**. Enforcement is hosted-only, transactional for creates, atomic reservation for concurrency/monthly (client.go:1666-1713).

**Background:** async batched Activity writer (drop-on-full), durable mail outbox (encrypted one-time tokens, 5-attempt dead-letter), hourly prune/reconcile, 6 h verified `VACUUM INTO` snapshots, graceful shutdown with writer flush.

---

### Release blockers

**TR-002 — Aggregate buffer memory amplification → single-account OOM (HIGH).**
`/v1` requests read the full body into memory (`io.ReadAll`, `MaxBytesReader` 32 MiB, client.go:608/620) *before* plan concurrency applies (`beginClientRequest` runs after `resolveRoute`, client.go:707/735). The only bound is `maxConcurrentBodyReads = 256` (bodyread.go:24) — 256 × 32 MiB ≈ 8 GB on a 4 GiB host. Response side: non-streaming upstream bodies buffer up to 64 MiB (`maxUpstreamNonStreamBytes`, client.go:27/1885) × 5 concurrent streams × unlimited accounts. A verified free user with one client key opens 256 connections uploading ≤32 MiB each → kernel OOM kill → all tenants down. Fix: lower the gate (e.g., 32-64) and/or the body cap (8 MiB ≈ 2 M tokens, far beyond any context window), plus a global semaphore or reduced non-stream response cap; a GOMEMLIMIT value does not bound live buffers.

**TR-001 — Hosted `opencode-free` rejection is frontend-only (HIGH).**
`HostedDisabled: true` (registry.go:85) is filtered only in `app.js:817`; `createProvider` (admin_providers.go:94) has no `descriptor.HostedDisabled` check, so a hosted user creates it by direct `POST /api/admin/providers` and relays through OpenCode's anonymous free tier under the operator's egress IP — contradicting the operator's own provider-terms decision (docs/provider_terms_review.md, 2026-10-01) and acknowledged in docs/hosted_status.md:162 as a "tracked follow-up." For a public launch this stops being deferrable: add the server-side rejection (one `if` in create/update).

---

### Findings

| ID | Severity | Category | Finding | ELI5 | Evidence / Code Reference | Exploit or Failure Scenario | Recommended Fix | Risk of Fix | Release Blocker |
|---|---|---|---|---|---|---|---|---|---|
| TR-001 | HIGH | Policy enforcement / abuse | Hosted `opencode-free` disabled only in UI, not server-side | The door is locked in the map app but not in the building | registry.go:85; admin_providers.go:94-98; app.js:817; hosted_status.md:162 | Hosted user POSTs provider type `opencode-free` directly; router relays anonymous OpenCode free traffic under operator IP → ToS violation, IP bans | Reject `descriptor.HostedDisabled` in createProvider (and updateProvider path), hosted mode only | None — implements an already-made decision; local mode untouched | **Yes (P0)** — **FIXED 2026-10-04** (create-path guard) |
| TR-002 | HIGH | DoS / resource management | Unbounded aggregate RAM: 256×32 MiB inbound buffers; 64 MiB×5×N outbound buffers | One free user can make the server eat all its memory and crash | client.go:608-630, 735; bodyread.go:24; client.go:27, 1885 | 256 conns × 32 MiB uploads → ~8 GB RSS → OOM kill, whole service down | Lower bodyReads gate + 32 MiB cap (8 MiB), global non-stream response budget/semaphore; add regression test on gate×size budget | Large-prompt users >8 MiB would break — that's ~2 M tokens, beyond any model; safe | **Yes (P0)** — **FIXED 2026-10-04** (gate 256→64, body 32→8 MiB, non-stream 64→16 MiB + 32-wide response gate held until body close) |
| TR-003 | MEDIUM | SaaS economics | Free plan seeded `monthly_requests = -1` (unlimited) | The free plan has no monthly cap at all | migration 042_plans.sql:24; store/plans.go; client.go:1687 | Unlimited routed requests per account; account farming multiplies; unbounded CPU/bandwidth/DB churn | Set a finite monthly cap via platform dashboard before public launch (data-only change) | None — operator data change; code already enforces | P1 — **FIXED 2026-10-04** (migration 050 sets the free plan to 20,000/month, conditionally so an operator-set cap is preserved; still operator-adjustable) |
| TR-004 | MEDIUM | Abuse (Needs Design Confirmation) | Client-key created/deleted notification events are exempt from the hosted cooldown → unbounded webhook relay | A user can make the server send endless POSTs to any public site they pick | notifications.go:132-134, 139-151, 46; admin_clients.go:181, 380-382 | Create/delete key cycles (each ≤2 webhooks, user-controlled "Client: {name}" body + auth header) to any validated public HTTPS URL at unbounded sequential rate; 64/8 caps bound concurrency only, not rate | Subject client-key events to a per-account hourly notification budget (or extend cooldown to them) | Medium — the exemption is documented as deliberate (rare admin churn); public beta breaks that assumption; confirm with Ben | P1 — **FIXED 2026-10-04** (Ben confirmed design: kept cooldown exemption for routing events, added hosted per-account rolling 30/hour budget across all events) |
| TR-005 | MEDIUM | Abuse / cost / data growth | No pending-account expiry; Turnstile off by default; signup mail per IP only | Anyone can make Tiller email any address and leave junk rows forever | identity.go:475-527 (pending, never reaped); maintenance.go:57-87 (no pending sweep); hosted_auth.go:102 (captcha opt-in), :135 (5/hr/IP) | Distributed IPs → unlimited verification mails (mail-provider spend) + unbounded `users`/`accounts`/`legal_acceptances` rows from never-verified signups | Enable Turnstile for public launch (ops); add pending-account GC sweep (>30 days unverified) | GC must never touch verified accounts; straightforward; mail quota per-email additionally | P1 — **FIXED 2026-10-04** (per-email 5/hour signup limiter on password + Google paths; 30-day pending-account GC in the hourly maintenance pass; Turnstile enablement remains an operator launch action) |
| TR-006 | LOW | Race / quota | `ReserveUsageCounter` fails open under SQLite write contention (BUSY_SNAPSHOT loser) | Under a burst, a few extra requests sneak past the monthly cap | usage_counters.go:41-55; client.go:1687-1694 (allow-on-error) | N concurrent requests at the cap boundary: loser's tx errors → request allowed without consuming a slot | Optional: retry once inside Reserve; or accept and document the deliberate fail-open | Retry could add latency; current behavior is a documented choice | P2 |
| TR-007 | LOW | Resource management | No per-account cap on `/api/admin/live` SSE connections | One user can open thousands of dashboard live-views | live.go:255-341, subscribe() | Authenticated user opens 10k SSE conns → goroutines + channels + broadcast work | Cap concurrent live subscribers per account (e.g., 5) | Degrades multi-tab power users mildly | P1 — **FIXED 2026-10-04** (8/account cap, 429 before SSE headers; constant by decision, not plan-configurable) |
| TR-008 | LOW | HTTP hardening | No HSTS header from the app | The site never tells browsers "HTTPS only, always" | server.go:1009-1029 (headers set) | TLS-stripping MITM relies entirely on proxy config | Add `Strict-Transport-Security` (or document mandatory proxy-level HSTS in the runbook) | None if done at app level for hosted only; local HTTP must be excluded | P2 — **FIXED 2026-10-04** (hosted-only `max-age=31536000`, no includeSubDomains) |
| TR-009 | LOW | Privacy / retention | `legal_acceptances` rows (IP + User-Agent) have no retention bound | Records of who signed up from where live forever | identity.go:532-540; maintenance.go:57-87 (not pruned) | Privacy policy must disclose indefinite retention of signup IP/UA — weak position for GDPR-ish requests | Align retention with the published privacy policy (e.g., prune after N years) | Legal-records retention is a policy call — confirm period | P2 |
| TR-010 | LOW | Session hygiene | Platform operator session TTL = 30 days sliding (inherited from user default) | The master console stays signed in for a month | identity.go New (`platformSessionTTL = userSessionTTL`); config.go:124 | Stolen operator cookie has a long window; revocation only on credential change/restart | Set a shorter platform TTL (e.g., 8-12 h) | Operators re-login more often; low impact | P2 — **FIXED 2026-10-04** (12 h default, `TILLER_PLATFORM_SESSION_TTL`) |
| TR-011 | INFO | Operational readiness | Load test at target rate never recorded (docs defer it) | The car hasn't been driven at highway speed yet | hosted_status.md Stage A; repo/tests/load/loadtest.py exists | Unknown SQLite write-contention ceiling under real beta traffic | Run and record the load test before announcing | None | P1 (ops gate) |
| TR-012 | INFO | Scale limit | Platform stats: per-account query loops (N+1 in users list; totals over all accounts) | The admin page gets slower as you get more popular | hosted_platform.go:109-131, 474-514 | Operator page latency grows linearly with account count | Fine for beta; aggregate SQL when account counts reach thousands | Refactor risk low; not needed now | P2 |
| TR-013 | INFO | Privacy note | `X-Real-IP` forwarded to OpenCode upstream on the anonymous free-shadow path (local-mode feature) | When using the free tier, the user's IP is sent to OpenCode | client.go:959-963 | Privacy disclosure matter for local-mode docs only | Document; moot in hosted once TR-001 lands | None | P2 — **DOCUMENTED 2026-10-04** (SECURITY.md) |
| TR-014 | MEDIUM | Under-logging / audit | No audit events for tenant-config mutations (provider create/update/credential replace, client-key rotate/permissions, settings incl. webhook URL/auth header); `http request` log lines carry no account ID | If someone tampers with a config, there's no record of who did it | recordAccountAudit call sites are identity-only (hosted_auth.go, hosted_account.go, hosted_google.go, hosted_webauthn.go); server.go:1038 (path-only request log) | Operator investigating "who swapped this provider credential / changed permissions?" has only DB `updated_at`, no actor, no time-ordered trail | Add account audit events for the mutation handlers; add `account_id` attr to the http request log (hashed or ID form) | More audit rows (already size-managed); no functional risk | P1 — **FIXED 2026-10-04** (audit events on provider/client-key/virtual/model/settings/OAuth mutations; request log gains `account_id`+`principal`) |

*Second adversarial pass additions: TR-002 (resource-abuser persona), TR-004 and TR-007 (malicious-free-user), TR-009 and TR-014 (incident-operator/privacy personas). TR-006 (cross-tenant/attacker with stolen key: quota is per-account, expected). Personas 1 and 3 produced no new findings beyond TR-005: unauthenticated surfaces are rate-limited and enumeration-resistant; every cross-tenant probe (store scoping, inflight/live/cooldown/usage caches, OAuth flow stores) is account-keyed and covered by the HTTP isolation matrix.*

---

### Logging & Privacy Assessment

**Over-logging: clean.** All 93 slog call sites follow the error-class-only pattern (`fmt.Sprintf("%T", err)`); the two raw `err.Error()` sites (client.go:722 resolveRoute, admin_oauth.go:457 discovery) carry DB/network text, never bodies or credentials. No passwords, tokens, cookies, Authorization headers, prompts, or responses are ever logged — including at debug level. Client-facing upstream errors pass through `sanitizeErrorText` + `redactProviderSecrets` (upstream_error.go:174-191). Mail outbox stores recipient + type + params (incl. new_email for warnings) readable for triage — documented, tokens encrypted and scrubbed on send.

**Bodies:** hosted mode can never persist request/error bodies (`GetLogErrorBodies` gated to non-hosted, client.go:656-658; enable attempt rejected, admin_settings.go:84-87). Migration 024 already purged historic bodies. Free-tier Activity retention clamps to 7 days.

**Under-logging:** TR-014 is fixed — config mutations now write account-audit events and `http request` lines carry the pseudonymous `account_id` + principal kind. Login audit events deliberately omit IP (privacy-positive); signup IP/UA live in `legal_acceptances` (TR-009).

**Retention/growth:** request logs pruned per key retention with plan clamp; audit pruned on its own window; tokens and outbox pruned hourly; snapshots verified + pruned (7 d). Log lines are bounded (activity error text 500 chars). No unbounded log growth path found.

---

### SaaS Abuse Assessment

- **CPU:** argon2 login verifies gated (4-slot admission = 256 MiB ceiling) + IP/email lockouts; bcrypt paths require a valid 128-bit selector first. Clean.
- **Memory:** **TR-002 is the exposure** (inbound 32 MiB buffers, outbound 64 MiB buffers, unbounded aggregate). Post-fix, the biggest remaining item is SSE live connections (TR-007).
- **Bandwidth/relay:** TR-004 (webhook relay via key churn) and TR-001 (OpenCode free relay). Notification admission (64 global / 8 per tenant) bounds concurrency only.
- **Email:** verification-mail flooding to arbitrary addresses at 5/hr/IP with Turnstile off by default (TR-005). Enable Turnstile; consider a per-address signup quota.
- **Database growth:** unverified-account accumulation has no GC (TR-005); Activity is well-bounded by retention clamps and drop-on-full writer.
- **Accounts:** signup is open (operator toggle), IP-budgeted, captcha-ready but opt-in; no global account cap — acceptable for beta if TR-003's monthly cap and TR-005's GC land.
- **Provider/API resources:** BYOK, so the operator's marginal per-request cost is bandwidth/CPU; TR-003 removes the only missing ceiling.

---

### Concurrency & SQLite Assessment

**Safe for a modest free public beta** with the documented posture: WAL + `synchronous=NORMAL`, `busy_timeout(5000)`, single-writer serialization, short transactions, tenant-tx lifetime lint (no tenant tx across I/O), batched Activity writer, maintenance mutex around VACUUM/backup. Atomic reservations (usage counters via conditional UPDATE; inflight via mutex+count) are correctly race-safe. Account deletion is terminal-first (`deleting` status blocks re-auth), retryable via durable cleanup tombstones, with in-process queue barriers for late Activity rows.

**Realistic limits:** (A) known failure mode TR-006 — quota check-then-increment txs can BUSY_SNAPSHOT-fail-open under bursts at the cap boundary (slight over-admission, deliberate allow-over-500 choice); (B) write throughput ceiling is unmeasured — the docs themselves defer the load test (TR-011), and the 4 GiB host makes TR-002's memory ceiling the first wall, not SQLite. PostgreSQL remains correctly deferred (no demonstrated requirement).

---

### Missing Test Coverage

1. Hosted `createProvider`/`updateProvider` rejection of `HostedDisabled` types (once TR-001 lands).
2. Regression test pinning the aggregate buffer budget (gate × cap ≤ host RAM) for the body-read gate and non-stream response cap (TR-002).
3. Pending-account GC correctness: never reaps verified/active accounts; idempotent restart (TR-005).
4. Hosted notification budget for non-routing events (TR-004) — create/delete churn cannot exceed the budget.
5. Concurrent `ReserveUsageCounter` at the cap under SQLite contention — documents the fail-open behavior or verifies the retry (TR-006).
6. Per-account live-SSE connection cap (TR-007).
7. Account-deletion audit events retained + config-mutation audit events written (TR-014).

---

### Positive Controls

Controls that are **already correctly implemented** — the review found no gaps in any of these:

- **Tenant isolation:** single SQL boundary (`internal/store`) with AST guard tests; table classification guard; two-account HTTP resource matrix; hosted fails closed on a missing account principal; all in-memory tenant state account-keyed; per-account immutable upstream clients (timeout setting can't cross tenants).
- **Secret custody:** AES-256-GCM, versioned format, AAD binds account/kind/id/field (ciphertext can't be moved between tenants); master key outside DB; locked state fails loud (`503 provider_credentials_locked`); hash-only one-time tokens; entropy-tiered KDFs (argon2id human 64 MiB with a 4-slot admission gate; bcrypt machine tokens) with lazy argon→bcrypt migration; rotation CLI with atomic key preservation and rollback.
- **Authn/authz:** `__Host-` cookies, Strict SameSite, HttpOnly, Secure (hosted unconditional); CSRF double-submit on all mutating session routes; same-origin + JSON content-type anti-form-CSRF on auth POSTs; `auth_generation` closes the authz-credential-change race; password reset invalidates sessions, outstanding tokens, and pending email changes; email change is verify-new-first with old-address warning, enumeration-resistant generic responses; Google flows use PKCE+state+nonce with single-use state and authoritative-email link gating; passkey ceremonies rate-limited and bounded; per-email limiter keys are HMAC'd.
- **SSRF:** hosted dial-time IP validation of every DNS answer (rebinding-safe), HTTPS/443 only, no userinfo, internal suffixes and all special-use ranges blocked incl. NAT64 translation ranges; provider/OAuth/discovery requests never follow redirects; webhook redirects revalidated with dial-time authority; env proxies disabled on the hosted transport.
- **Input handling:** `DisallowUnknownFields` everywhere; bounded bodies (8 KiB auth, 64 KiB WebAuthn, 32 MiB JSON, 1 MiB SSE lines); body-read deadline against slowloris; pagination and offset caps; parameterized SQL throughout (interpolated fragments are internal constants only); frontend escapes via `h()` consistently and renders error bodies via `textContent`; CSP without `unsafe-inline` scripts; custom-site path traversal blocked and reserved routes protected.
- **DoS controls (partial):** notification admission bounds, body-read deadline, upstream idle timeout (5 min) with afterfunc cancel, SSE keepalive unification, drop-on-full Activity queue, limiter memory caps with lockout-preserving eviction.
- **Ops:** live/ready/version health; graceful shutdown draining HTTP + Activity writer; verified snapshots; security.txt; AGPL source link; `X-Tiller-Secret-Material` on backups; backup export correctly local-only (server.go:612-616).
- **Mail:** CRLF guards on all headers, TLS required (STARTTLS/implicit), tokens encrypted at rest and scrubbed on send, dead-letter audit + operator queue visibility.

---

### Prioritised Fix Plan

**P0 — Must fix before public exposure**
1. TR-002: bound aggregate body/response buffering (lower `maxConcurrentBodyReads` + body cap; global non-stream response budget) + regression test.
2. TR-001: server-side `HostedDisabled` rejection in provider create/update (hosted mode).

**P1 — Should fix before announcing/promoting**
3. TR-003: set a finite `monthly_requests` on the free plan — **FIXED** (migration 050 defaults it to 20,000/month).
4. TR-005: enable Turnstile (ops) — **operator action, still open**; the code side (per-email limiter + GC) is done.
5. TR-004 — FIXED.
6. TR-014: audit events for tenant-config mutations + account attribution in request logs — FIXED.
7. TR-007: per-account live-SSE connection cap — FIXED.
8. TR-011: run and record the deferred load test (ops gate) — **still open**.

**P2 — Can safely follow after beta launch**
9. TR-006 (quota fail-open retry or documented acceptance), TR-009 (legal_acceptances retention), TR-012 (platform stats aggregation) remain open; TR-008 (HSTS), TR-010 (platform TTL), TR-013 (docs note) are FIXED.

---

**Verdict re-check after the adversarial pass (historical):** the second pass upgraded the verdict from "GO WITH MINOR FIXES" to **NO-GO UNTIL BLOCKERS FIXED** — TR-002 is a reliable, cheap, single-account whole-service outage, and TR-001 voids an explicit operator policy decision on day one. Both are small, low-risk fixes; nothing found rises to architectural rework, and the deferred-items list (PostgreSQL, `app.`/`api.` origin split, external monitoring) remains correctly deferred.

**(Superseded 2026-10-04:** the two blockers were fixed — see the Release verdict section at the top for the current GO WITH MINOR FIXES verdict and the remaining operator gates.**)**