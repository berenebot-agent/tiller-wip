# Changelog

All notable changes to Tiller Router are recorded here. This project follows
semantic versioning conventions where practical; the alpha API and deployment
behavior may still change.

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
  tested. See [provider support](docs/provider-support.md).
- Provider credential encryption at rest, multi-user/SaaS operation, and
  Kubernetes deployment are outside this release's scope.
- Model capabilities and streaming/tool behavior depend on the selected
  provider and model; verify them before production use.
