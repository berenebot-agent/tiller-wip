# Changelog

All notable changes to Tiller Router are recorded here. This project follows
semantic versioning conventions where practical; the beta API and deployment
behavior may still change before a stable `1.0`.

## [Unreleased]

## [0.1.0-beta.4] - 2026-10-09

Fourth public beta. Re-publishes the 0.1.0-beta.3 feature set with a release-gate
fix so the multi-architecture image and `latest` tag publish correctly, plus the
changes below accumulated since beta.3.

### Added

- **DNS resolver fallback.** When the configured DNS transport or servers fail,
  the router can retry resolution against an ordered fallback list (Cloudflare
  then Google by default) via `TILLER_DNS_FALLBACK_SERVERS`. Valid replies —
  including NXDOMAIN and empty answers — never fall through; only transport or
  server failures do. Set to `off` to disable, or to a comma-separated list of
  `IP`/`IP:port` resolvers to override.

### Changed

- **First-run docs and compose simplification.** README first-run guidance now
  points at the browser setup wizard as the primary path, and the compose example
  is trimmed accordingly.

### Fixed

- **Quota reset countdown over 24h.** Reset windows longer than a day now render
  as `Nd Yh left` instead of a large hour count (e.g. `30h 0m left`).
- **Release gate failing on the hosted pprof config test.** The test wrote to
  the default `/data` directory, which is not writable on the CI runner, so the
  `v0.1.0-beta.3` release gate failed at `go test` and the image-publish job was
  skipped — `latest` stayed pinned to beta.2. The test now uses `t.TempDir()`.
- **CI masking non-zero test/vet/build exits.** The `go test`, `go vet`, and
  `go build` steps piped output through `tee` without `pipefail`, so a failing
  step reported `tee`'s success. CI now fails correctly on a red gate.

## [0.1.0-beta.3] - 2026-10-05

Third public beta. Highlights: a hosted multi-tenant product shell with plans
and quotas, passkeys and a unified local/hosted operator identity, first-run
setup for self-hosted installs, thin Go and Python gateway clients, and a
security-hardening pass from the pre-SaaS release review.

### Added

- **Local first-run setup.** A local instance started without
  `TILLER_USERNAME`/`TILLER_PASSWORD` now boots instead of failing: the first
  visitor gets a one-time setup page to create the administrator credential
  (8+ characters, argon2id hash-only) and is signed straight into a guided
  onboarding wizard. With no admin configured, `GET /api/runtime` reports
  `setup_required` and the router logs a warning each boot until the instance
  is claimed. The setup endpoint is same-origin checked, per-IP rate-limited,
  and one-shot (404 after the claim). After setup, the reused hosted onboarding
  wizard (provider → client key → optional virtual route → connect snippet)
  auto-opens with the same completion semantics as hosted. Environment
  credentials still skip setup entirely and now hide the wizard, matching the
  previous env-admin experience.
- **DB-backed local admin credential.** Local login verifies the stored
  credential (argon2id) instead of comparing environment strings. Environment
  values seed/override it at boot (rewriting the hash and revoking sessions);
  the wizard writes it directly, so a wizard-created credential survives
  restarts with no environment set. The administrator username is persisted
  for display (`platform_settings.admin_username`).

- **Thin Go and Python gateway clients** under `clients/`, using Tiller's public
  HTTP interface or an explicit OpenRouter profile. Includes normalized catalogs
  and tri-state capability filters, native Chat/images/tools/JSON/reasoning
  requests, streaming with terminal-error detection, usage and safe errors,
  shared fixtures, runnable examples, installed-wheel tests and real Tiller HTTP
  integration coverage.
- **Optional consent-gated hosted analytics.** The platform dashboard can enable
  web analytics for hosted pages (Umami primary; Plausible or a custom https
  script URL). Disabled by default. When enabled, the script is requested only
  after the visitor accepts the bottom consent banner, and the operator's origin
  is added to the hosted Content-Security-Policy. Consent is stored client-side;
  no server-side analytics collection is added. Matomo is not supported.
- **Hosted product shell (private alpha).** The hosted product now has plan
  entitlements (creation limits, concurrent-stream and monthly-request limits,
  Activity retention clamp), all enforced only in hosted mode and operator-
  tunable from the platform dashboard. A first-run setup wizard (provider →
  target → client key → curl snippet) opens on first hosted login. Completion is
  derived from Activity (a successful routed request), from an account being
  configured with at least one provider and at least one client key, or from the
  user skipping setup — after which the wizard stops auto-opening and its
  top-bar "Get started" button is removed. Email verification now signs the user
  straight in.
- **Published legal pack.** First-draft Terms of Service and Privacy Policy are
  embedded in the binary, seeded at startup, editable from the platform
  dashboard, and served publicly (e.g. `/legal/terms`). The Terms incorporate
  the acceptable-use rules; the Privacy Policy documents the subprocessors and
  the security/data-handling posture. Signup requires an explicit Terms
  acceptance recorded with a timestamp.
- **Account data export.** Hosted customers can download an account-scoped ZIP
  containing their configuration, Activity metadata, and audit history. Provider
  credentials and client secrets are never included.
- **`security.txt`** served at `/security.txt` and `/.well-known/security.txt`,
  and an AGPL source/version link in the hosted footer.
- **Load-test harness** (`tests/load/loadtest.py`) that measures sustained
  request rate, failure count, and router latency percentiles against a mock
  upstream without spending provider credits.
- **Hosted Account page.** A hosted-only Account tab in Settings lets customers
  view their identity, change password, change email (verify-new-first with a
  warning to the current address), sign out everywhere, and delete their account
  instantly. All sensitive actions re-authenticate with the current password,
  passkey assertion, or Google proof.
- **Hosted passkeys (WebAuthn).** Passwordless-capable passkey sign-in and
  registration on hosted deployments with a `TILLER_PUBLIC_URL` (RPID is its
  hostname, origin is the public origin; local mode has no passkey endpoints).
  Users can register multiple discoverable passkeys, rename and remove them from
  the Account page, and make a passkey their only sign-in method (refused when
  removing the last method would lock the account out). Passkey-only accounts
  re-authenticate sensitive operations (change password, change email, delete
  account) with an assertion instead of a password, and can still recover via
  email password reset if every device is lost. Synced passkeys (iCloud
  Keychain, Google Password Manager, Windows Hello) are supported: the
  backup-eligible/backup-state flags and transports round-trip through storage.
  Attestation is not requested and no extensions are used. The login and
  account ceremony endpoints are rate-limited, body-bounded, same-origin, and
  (for the authenticated half) CSRF-protected; the management endpoints refuse
  when passkeys are not configured. Notices and license copies updated
  (go-webauthn v0.18.2 and its transitive set).
- **Durable transactional mail outbox.** Signup, verification, password-reset,
  and email-change messages are queued in the same transaction that creates the
  one-time token and delivered by a retrying background worker. The one-time
  token is encrypted at rest and scrubbed on send, and dead letters surface on
  the platform dashboard. The delivery log shows the actual delivery time,
  counts successful sends as attempts, and displays Brevo message IDs for
  provider-side delivery correlation.
- **Activity living-pane graph.** The Activity view now renders live request
  legs as a graph (self-hosted D3, no CDN) with an active-only pane, per-client
  legs, and click-through from graph nodes into the request dialog. Cooldown
  state is shown inline.
- **Sanitized upstream provider errors surfaced.** Upstream failures now
  surface a sanitized, actionable error instead of an opaque failure.
- **Provider logos and a searchable provider picker.** Provider cards and the
  Add provider flow show locally served brand marks (self-hosted LobeHub
  `@lobehub/icons-static-svg` SVGs, no CDN, plus an original in-house mark for
  the generic OpenAI-compatible type) keyed by provider type, covering every
  catalogued provider type and falling back to a readable monogram only for
  unknown or future types. Adding a provider now opens an enlarged,
  searchable, keyboard-operable catalogue picker that shows the full logo grid
  (commonly used types first) with auth/protocol context, then advances to the
  existing configuration fields; editing an existing provider is unchanged.

### Hardened (pre-SaaS release review: docs/pre_saas_release_review.md)

- **Server-side enforcement of hosted provider-terms decisions (TR-001).**
  Provider types marked `HostedDisabled` (today: `opencode-free`) are rejected
  by the provider-create endpoint in hosted mode, not just hidden in the hosted
  add-provider UI — a direct API call can no longer create a type an
  operator's provider-terms review has disabled. Local mode is unchanged.
- **Bounded request/response buffering (TR-002).** The inference body cap
  drops from 32 MiB to 8 MiB (≈2 M tokens of text — beyond every model's
  context window, so no legitimate request is affected), the process-wide
  body-read gate from 256 to 64 concurrent reads, the non-streaming upstream
  response cap from 64 MiB to 16 MiB, and a new global gate admits at most 32
  concurrently buffered non-streaming responses (holding each slot until the
  buffered body is closed). Worst-case aggregate buffering is now ~1 GiB
  instead of unbounded, so a single client-key flood can no longer OOM the
  container. Oversized requests get the same `request_too_large` 400 as
  before, with the limit named in the message.
- **Hourly hosted notification budget (TR-004).** In hosted mode one account
  may deliver at most 30 webhook notifications per rolling hour across all
  events. The per-event cooldown for routing events is unchanged; this closes
  the unbounded-rate relay through client-key create/delete cycles (whose
  events are deliberately cooldown-exempt). Local mode keeps unthrottled
  admin events. Excess deliveries are dropped best-effort and logged.
- **Signup mail bound per address (TR-005).** The signup endpoint (password
  and Google paths) now also charges a 5/hour per-address fixed window,
  HMAC-keyed like the login limiter, so rotating source IPs cannot turn
  signup verification mail into a mail cannon against a single mailbox.
- **Abandoned-signup reclamation (TR-005).** Never-verified (`pending`)
  accounts older than 30 days are swept by the hourly maintenance pass:
  the pending account, its user row, its legal acceptance record, and its
  unsent mail are removed. Verified/active/suspended/deleting accounts are
  never touched. Without this, a public service would accumulate abandoned
  signups (and their unique-email hold) indefinitely.
- **Tenant-configuration audit trail (TR-014).** Provider create/update/
  delete/credential-replace/refresh and OAuth connect/disconnect, client-key
  create/update/rotate/delete/permission changes, virtual group/model
  create/update/delete, manual model add/delete, and settings updates now
  write account-scoped audit events with actor, target, and field names —
  never secret values (credentials, client-key plaintext, webhook URLs, or
  the notification auth header). The `http request` log line also carries the
  pseudonymous `account_id` and authenticated principal kind, so an operator
  can correlate a request with the account it served.
- **Per-account live-SSE cap (TR-007).** The dashboard live stream admits at
  most 8 concurrent connections per account (a constant DoS bound, not a plan
  entitlement); the 9th is refused with a `429` before any SSE headers are
  written, and a freed slot is reusable.
- **HSTS on hosted deployments (TR-008).** Hosted responses send
  `Strict-Transport-Security: max-age=31536000` (no `includeSubDomains`).
  Local mode, which serves plain HTTP on the LAN by design, never sends it.
- **Shorter platform-operator session lifetime (TR-010).** The hosted
  platform console session now defaults to a 12 h sliding window instead of
  the 30-day customer default, configurable via `TILLER_PLATFORM_SESSION_TTL`.
- **Free-plan monthly default (TR-003).** The `free` plan now defaults to a
  finite 20,000 requests/month (migration 050, applied conditionally so an
  operator-set cap is preserved). The cap remains operator-adjustable from the
  platform dashboard, effective immediately; `-1` still means unlimited.
- **Load harness large-body probe (TR-011).** `tests/load/loadtest.py` gained
  `--body-bytes`, and `docs/load_test.md` now carries public-beta pass criteria
  plus probes for the review's admission controls (body-read gate, concurrent-
  stream cap, live-SSE cap, multi-account isolation). Regression tests pin the
  hosted `HostedDisabled` provider rejection and the inference buffer budget.

### Fixed

- **Local admin credential change no longer breaks login.** Changing
  `TILLER_USERNAME` after the local operator row existed updated only the
  password hash, leaving the synthetic email bound to the old username; the new
  username was rejected before the password was checked, and the legacy
  fallback could not re-materialise the row (`EnsureLocalOperator` returned
  `ErrLocalOperatorExists`). The boot-time sync now updates the synthetic
  username/email, the credential hash, and the auth generation together (a
  no-op when nothing changed), so `TILLER_USERNAME`/`TILLER_PASSWORD` behave as
  the documented seed/override/recovery path again. Regression: boot as A,
  restart the same database as B/password B — B works, A does not, and A's
  session is revoked.
- **Local → hosted conversion no longer collides after a local boot.** A local
  install that had materialised its operator `users` row owned `LocalAccountID`,
  so hosted bootstrap's `owner_user_id IS NULL` claim failed with
  `ErrBootstrapCollision` and startup aborted. Bootstrap now recognises the
  local operator row and converts **that same user in place** — preserving
  `users.id` (so registered passkeys survive), replacing the synthetic
  `@local.invalid` email with the supplied hosted email, and re-hashing the
  credential with hosted bare-password semantics. Regression: register a passkey
  in local mode, reopen the database in hosted mode, then verify hosted password
  login, preserved tenant data, passkey login under the same RP/origin, and an
  idempotent hosted restart.
- **Notification budget panic on first delivery (TR-004 follow-up).** The
  per-account hourly notification budget dereferenced a nil entry for an
  account's first delivery — and again after its entry aged out of the window —
  panicking the fire-and-forget delivery goroutine. That panic is unrecovered
  and would have terminated the whole process, so any hosted account that
  enabled notifications and triggered a webhook event could crash the service.
  The budget now allocates the entry on first use. Unit tests cover first
  delivery, budget exhaustion and window expiry, and per-account isolation.
- **Restored the Google signup per-IP throttle.** `completeGoogleSignup` lost
  its per-IP signup limiter when the per-address budget was added; it now
  charges both the IP and the proved-email budgets, matching the password
  signup path.
- **Silent mismatch between hosted customer bootstrap and the environment.**
  `TILLER_USERNAME`/`TILLER_PASSWORD` on a fresh hosted install are ignored by
  design (they are one-time migration input for an existing local database, and
  `database.Open` records the completed bootstrap before the store is asked), but
  nothing said so: the operator saw only `Invalid email or password` at the login
  screen, which points at the password rather than the configuration. The router
  now warns when those variables are set on a fresh hosted install, and logs an
  info line when an existing local account is converted into a verified hosted
  customer, so both branches of the decision are observable at startup. No
  behavior changed in either case.
- **Hosted Codex subscription sign-in.** Codex now connects with OpenAI's
  device authorization flow (the flow `codex login --device-auth` uses for
  remote/headless clients) instead of the browser PKCE flow. The PKCE flow's
  redirect URI must be the registered loopback callback, which a hosted router
  cannot receive; OpenAI rejected the hosted URL with its generic
  `unknown_error` page and sign-in never completed. Device authorization
  exchanges through OpenAI's own registered device callback, so it works from a
  hosted server with no client registration. Claude subscription sign-in keeps
  the existing paste-back flow.
- **Hosted release-readiness paths.** Hosted SPA entry URLs now retain their
  requested flow without redirects or initialization errors; verification
  resend/reset recovery, hosted account search/paging, deletion retry, and
  local-only backup boundaries are complete. Platform settings now validate and
  persist atomically, OAuth disconnects cannot be undone by stale work, Activity
  cleanup and shutdown are durable, audit retention runs independently, trusted
  proxy login limits are consistent, bootstrap/signup email validation is strict,
  and active cached sessions renew their persisted expiry.
- **Ordered fallback now survives empty or errored 2xx streams.** A target that
  returns HTTP 200 but delivers an explicit upstream stream error, or ends
  without any assistant output before client-visible bytes, is treated as a
  failed attempt so the chain advances to the next configured target. Both new
  classes (`upstream_stream_error`, `empty_response`) also open a target
  cooldown. Previously Tiller committed to the first 2xx header and relayed the
  empty response to the client, so a broken upstream could stall the whole
  chain.
- **Codex / SSE robustness.** Preserved upstream SSE negotiation, exact SSE
  accept handling, streaming keepalives, and resolution of UI reasoning aliases
  to wire efforts.
- **Activity pane correctness.** No more phantom middle-lane routes for direct
  real-model requests; orphan models, graph stalls, and skipped-leg rendering
  fixed; models added after the pane loads now resolve; redundant real-model
  sublabels dropped and long labels wrap.
- **Parallel requests from one client key no longer collapse.** Live
  in-flight activity is now tracked per (client key, route) instead of per
  client key alone, so an OpenCode/Tiller client running two routes at once
  (for example `main` → Claude and `coding` → Codex) keeps both virtual-model
  spinners and both Activity graph legs lit. The client status roundel still
  consumes a folded per-client aggregate. Previously a second concurrent route
  overwrote the first route's identity on the single per-client ticket.
- **Admin dialog guards.** Stale entity submits no longer close a reopened
  dialog; search fields aligned across views; activity dialog scroll resets on
  open.

### Changed

- **Admin Usage cold-load performance.** Route attribution is indexed
  (migration `027`) and the usage snapshot is cached, removing the cold-load
  lag on the Usage view.
- **Admin pages render before usage loads.** The Real Models, Virtual Models,
  and Clients views no longer block their first paint on the usage/health
  aggregation. Catalogue rows render immediately; token and cache cells show a
  loading spinner until the live SSE snapshot (or the fallback fetch) arrives
  and patches them in place. Previously every view awaited the usage endpoint —
  the slowest call — before rendering anything, and unknown cells were
  indistinguishable from a "no traffic" dash. Because the Real Models table
  defaults to a usage-based sort, it is re-sorted once when usage first arrives
  (honouring the user's current column and direction) so it never sits in
  catalogue order under a "1h ↓" header.
- **Catalogue capability docs.** README documents the limits of catalogue
  capability metadata.

## [0.1.0-beta.2] - 2026-09-08

Second public beta. Highlights: sign in to your existing AI subscriptions,
finer control over reasoning, automatic cooldown of failing fallback targets,
manually added models, and a control panel that updates live.

### Added

- **Subscription sign-in:** connect Codex (ChatGPT), Claude Code, and GitHub
  Copilot accounts directly, alongside API-key providers.
- **Reasoning controls:** choose how much reasoning a model uses, per model and
  per route target, with a clear warning when a target can't honour it.
- **Fallback cooldown:** when a target in an ordered fallback chain keeps
  failing, Tiller temporarily parks it and moves on, then brings it back once
  it recovers. The state is visible live in the control panel.
- **Add your own models:** if discovery doesn't list a model, add it manually
  and Tiller fills in the details it can.
- **Live control panel:** activity, route status, and usage counters update as
  requests happen, with no refresh needed.
- **Optional detailed error logging:** off by default; when enabled, failed
  requests and provider errors are kept (capped in size) to help you debug.
- **Better activity details:** clearer error messages, one row per attempt, and
  click any request to see the full error.
- **OpenCode Free** support, plus OpenCode Zen and Go.

### Changed

- **Easier first run:** a fresh install now works with no manual permission
  steps — Tiller fixes its data directory itself, then runs as a non-root user.
  Stronger lockdown is still available as an opt-in for internet-facing setups.
- New settings for the runtime user, secure admin cookies, and log level.
- A faster, more reliable test suite with clearer logs.

### Fixed

- Reasoning settings now apply correctly across providers and protocols.
- Sign-in and token refresh for subscription providers are more robust.
- Activity now attributes requests and shows errors correctly.
- Failing fallback targets no longer cool down when the client cancels the
  request, and recover as soon as they succeed.
- Virtual targets: retired targets are kept, unavailable targets show a clear
  error instead of crashing, and the target picker behaves better.
- Many control-panel polish fixes.

### Security

- Provider error details are hidden from logs unless you explicitly enable
  detailed error logging.

## [0.1.0-beta.1] - 2026-09-01

Initial public FOSS beta release.

Tiller Router moves from alpha to beta: the routing core, deployment model and
security posture are treated as more settled, with a clear 1.0 path.

### Fixed

- On first launch with a fresh bind-mounted `./data` directory (created as root
  by rootful Docker), startup now logs an actionable one-time remediation
  (`sudo chown -R 65532:65532 ./data`, or `TILLER_UID`/`TILLER_GID` in `.env`)
  instead of the cryptic `open database: chmod /data: operation not permitted`.
  The underlying error now surfaces a detectable
  `ErrDataDirUnwritable` sentinel.

## [0.1.0-alpha.1] - 2026-09-01

Initial public FOSS alpha release.

### Added

- Docker Compose deployment with persistent bind-mounted `./data` storage and
  a minimal non-root runtime image.
- Authenticated admin UI and API for provider, model, client-key, permission,
  virtual-route, activity, usage, and notification management.
- Real and virtual model routing, ordered virtual fallback, route diagnostics,
  and immediate configuration updates.
- OpenAI Chat Completions and Responses, Anthropic Messages, and compatible
  request/response translation where the selected provider supports it.
- Provider descriptors and model discovery for the supported provider families,
  optional models.dev metadata enrichment, and activity JSON/CSV export.
- Hash-only client-key storage, admin sessions with CSRF protection, rate
  limiting, security-conscious request logging, and plain-text webhook
  notifications.
- Compatibility probes for common OpenAI/Anthropic SDK and CLI workflows,
  including restart persistence checks.

### Known limitations

- This is an alpha release: interfaces, provider behavior, and operational
  defaults may change between releases.
- Provider integrations are contract-tested with local mocks; external provider
  accounts and every provider/model combination are not continuously live
  tested. See the provider matrix in the README.
- Provider credential encryption at rest, multi-user/SaaS operation, and
  Kubernetes deployment are outside this release's scope.
- Model capabilities and streaming/tool behavior depend on the selected
  provider and model; verify them before production use.
