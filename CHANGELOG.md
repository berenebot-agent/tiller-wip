# Changelog

All notable changes to Tiller Router are recorded here. This project follows
semantic versioning conventions where practical; the beta API and deployment
behavior may still change before a stable `1.0`.

## [Unreleased]

### Added

- **Hosted product shell (private alpha).** The hosted product now has plan
  entitlements (creation limits, concurrent-stream and monthly-request limits,
  Activity retention clamp), all enforced only in hosted mode and operator-
  tunable from the platform dashboard. A first-run setup wizard (provider →
  target → client key → curl snippet) opens on first hosted login and completion
  is derived from Activity. Email verification now signs the user straight in.
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
  instantly. All sensitive actions re-authenticate with the current password.
- **Durable transactional mail outbox.** Signup, verification, password-reset,
  and email-change messages are queued in the same transaction that creates the
  one-time token and delivered by a retrying background worker. The one-time
  token is encrypted at rest and scrubbed on send, and dead letters surface on
  the platform dashboard.
- **Activity living-pane graph.** The Activity view now renders live request
  legs as a graph (self-hosted D3, no CDN) with an active-only pane, per-client
  legs, and click-through from graph nodes into the request dialog. Cooldown
  state is shown inline.
- **Sanitized upstream provider errors surfaced.** Upstream failures now
  surface a sanitized, actionable error instead of an opaque failure.

### Fixed

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
