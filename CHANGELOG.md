# Changelog

All notable changes to Tiller Router are recorded here. This project follows
semantic versioning conventions where practical; the beta API and deployment
behavior may still change before a stable `1.0`.

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
