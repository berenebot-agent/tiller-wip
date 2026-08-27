# Tiller Router — Post-V1 Roadmap

**Project:** Tiller Router  
**Document purpose:** Capture deliberately deferred features without allowing them to expand the V1 implementation scope.

This roadmap is not part of the V1 definition of done.

---

# 1. Roadmap Principles

Tiller Router should remain a model-selection proxy first.

New features should be accepted only when they strengthen one of these jobs:

1. Model discovery.
2. Client-specific model visibility.
3. Stable virtual model identities.
4. Reliable upstream routing.
5. Safe operation of the router.

Avoid turning Tiller Router into a general-purpose AI platform.

Before adding a feature, ask:

> Does this improve model selection/routing, or are we recreating LiteLLM/Bifrost?

---

# 2. Priority 1 — Routing Reliability

## 2.1 Virtual-model fallbacks

Allow a virtual model to specify an ordered target list.

Example:

```text
main/coding
  1. openrouter/anthropic/claude-sonnet
  2. openai/gpt-5
  3. ollama-local/qwen3
```

Required future decisions:

- Which HTTP statuses trigger fallback?
- Do timeouts trigger fallback?
- Do rate limits trigger fallback?
- Does model-not-found trigger fallback?
- How are streaming failures handled after bytes have already been sent?
- How is target selection surfaced to administrators?

V1 intentionally has no fallback.

---

## 2.2 Provider health state

Add background health checks that are separate from model discovery.

Possible states:

```text
healthy
degraded
unreachable
auth-failed
rate-limited
unknown
```

Health state should inform administrators first.

Automatic routing decisions based on health should remain a later opt-in feature.

---

## 2.3 Retry policy

Add conservative configurable retry handling for safe failure classes.

Must avoid:

- Duplicate tool calls.
- Duplicate billable requests.
- Retrying after streaming has begun.
- Retrying non-idempotent operations blindly.

---

# 3. Priority 2 — Operational Observability

## 3.1 Metadata-only request audit

Add optional request metadata logging without storing prompts/responses.

Possible fields:

```text
timestamp
client_key_id
requested_model
resolved_provider
resolved_model
protocol
streaming
http_status
latency_ms
input_tokens
output_tokens
provider_request_id
```

Do not log prompt or response bodies by default.

---

## 3.2 Usage statistics

Optional aggregate statistics:

- Requests per client key.
- Requests per virtual model.
- Requests per real provider/model.
- Token counts.
- Error rates.
- Latency percentiles.

Prefer SQLite initially.

Only consider an external analytics store if SQLite demonstrably becomes inadequate.

---

## 3.3 Admin dashboard

Possible widgets:

- Provider status.
- Recent routing errors.
- Most-used virtual models.
- Client-key request counts.
- Recent catalogue changes.
- Retired target warnings.

Avoid a large dashboard in V1.

---

# 4. Priority 3 — Credential Hardening

## 4.1 Provider-credential encryption at rest

Introduce encryption for provider credentials stored in SQLite.

Recommended model:

- Master encryption key supplied via environment or mounted secret.
- Application-level authenticated encryption.
- Per-record nonce.
- No encryption key stored in SQLite.

Rotation design should be considered before implementation.

---

## 4.2 Hash-hardening options for client keys

V1 stores hash-only client secrets.

Future improvements may include:

- Memory-hard KDF if justified.
- Key prefixes/identifiers for faster lookup.
- Multiple valid key hashes during planned rotations.

---

## 4.3 Graceful client-key rotation

Allow temporary overlap:

```text
old key valid
new key valid
```

for a configurable migration window.

Potential UX:

```text
Rotate now
Keep old key valid for:
  0 min
  15 min
  1 hour
  24 hours
```

V1 intentionally uses immediate replacement only.

---

# 5. Priority 4 — Permission Enhancements

## 5.1 Permission templates

Create reusable client profiles.

Examples:

```text
Coding Agents
Production Agents
Local Models Only
Cheap Models
Virtual Models Only
```

A new client key could inherit a template.

Need clear semantics for whether future template changes propagate.

---

## 5.2 Bulk permission actions

Possible admin actions:

- Enable all existing models in a provider group.
- Disable all existing models in a provider group.
- Enable models matching a search/filter.
- Clone permissions from another client key.

These are UI convenience features.

They must not change the underlying V1 semantics where the group-level feeder controls only newly discovered models.

---

## 5.3 Tag-based model permissions

Allow models to be tagged:

```text
coding
reasoning
cheap
local
experimental
vision
```

Client permissions could optionally reference tags.

Keep explicit per-model permissions as the source of truth.

---

# 6. Priority 5 — Catalogue Management

## 6.1 Catalogue-diff history

Track:

- Newly discovered models.
- Disappeared models.
- Reappeared models.
- Provider metadata changes.

Useful for OpenRouter-style providers with fast-changing catalogues.

---

## 6.2 Model metadata normalization

Store normalized metadata such as:

- Context window.
- Input/output pricing.
- Tool support.
- Vision support.
- Reasoning support.
- Provider metadata.
- Deprecation state.

Do not make V1 depend on consistent metadata across providers.

---

## 6.3 Search and filtering

Enhance admin catalogue search by:

- Provider.
- Model family.
- Capability.
- Cost.
- Availability.
- Tags.

---

# 7. Priority 6 — Virtual Model Enhancements

## 7.1 Compatibility aliases

Allow a virtual model to expose multiple client-facing names.

Example:

```text
main/coding
aliases:
  coding
  sonnet
  default
```

Potentially useful for clients with hard-coded model names.

Requires explicit collision rules.

---

## 7.2 Rename compatibility window

When renaming:

```text
main/coding -> main/dev
```

optionally retain the old name temporarily as an alias.

V1 intentionally treats rename as breaking.

---

## 7.3 Weighted routing

Possible future virtual model:

```text
main/general
  90% openai/gpt-5
  10% openrouter/anthropic/claude
```

This is out of scope until deterministic routing and observability are well proven.

---

## 7.4 Rules-based routing

Potential rules:

- By client key.
- By request size.
- By protocol.
- By required capability.
- By time of day.
- By provider health.

This is intentionally far beyond V1.

---

# 8. Priority 7 — Rate Limits, Quotas and Budgets

Possible future controls:

- Requests/minute per key.
- Tokens/minute per key.
- Daily/monthly budget.
- Provider budget.
- Model budget.
- Concurrent request limits.

These require reliable usage accounting first.

---

# 9. Priority 8 — Protocol Expansion

V1 includes:

- OpenAI Chat Completions.
- OpenAI Responses.
- Anthropic Messages.

Possible future surfaces:

- Embeddings.
- Images.
- Audio.
- Files.
- Batch APIs.
- Realtime APIs.
- Reranking.
- Provider-specific extensions.

Each should be added only when required by actual clients.

---

# 10. Priority 9 — Authentication and Administration

## 10.1 Multiple admin users

Possible later support:

- Multiple admin accounts.
- Read-only role.
- Operator role.
- Full admin role.

V1 has one environment-defined admin account.

---

## 10.2 SSO

Possible:

- OIDC.
- Authentik.
- Authelia.
- Entra ID.
- Google Workspace.

Not required for a private-LAN V1 deployment.

---

## 10.3 API tokens for automation

Separate admin automation tokens from human login sessions.

Possible scopes:

```text
providers:read
providers:write
models:read
virtuals:write
clients:write
backup:read
```

---

# 11. Priority 10 — Backup and Recovery Enhancements

## 11.1 Scheduled backups

Add optional scheduled SQLite backups under:

```text
/data/backups/
```

Potential retention:

```text
daily x 7
weekly x 4
monthly x 6
```

Keep implementation simple.

---

## 11.2 Configuration-only export

Add a portable JSON/YAML export that omits runtime/cache data.

Potential contents:

- Provider definitions.
- Virtual mappings.
- Client metadata.
- Permission structure.

Provider credentials should be optionally excluded.

Client plaintext keys can never be exported because they are not stored.

---

## 11.3 Restore validation

Before restoring:

- Validate schema version.
- Validate referential integrity.
- Preview affected provider/virtual/client counts.
- Preserve previous DB as rollback copy.

---

# 12. Priority 11 — Provider Enhancements

## 12.1 Additional native adapters

Potential providers:

- Google Gemini.
- Vertex AI.
- AWS Bedrock.
- Azure OpenAI.
- Groq.
- Mistral.
- xAI.
- Together.
- Fireworks.
- Cerebras.
- Cloudflare AI Gateway.
- Other OpenAI-compatible providers.

Prefer generic OpenAI compatibility where it is sufficient.

---

## 12.2 Multiple credentials per provider instance

Potentially allow one logical provider instance to contain a credential pool.

This should only be added if there is a real need for provider-side load distribution or quota management.

The V1 pattern of multiple uniquely named provider instances is simpler and should remain valid.

---

# 13. Priority 12 — Advanced Routing Safety

## 13.1 Capability validation

Warn or block when a virtual model target cannot support expected features.

Examples:

- Tool calling.
- Vision.
- Reasoning fields.
- Large context.
- Responses API.

This may use discovered model metadata or adapter-declared capabilities.

---

## 13.2 Protocol compatibility matrix

Expose a matrix such as:

```text
Provider/model       Chat   Responses   Messages   Tools   Streaming
...
```

Useful when choosing virtual targets.

---

# 14. Priority 13 — Deployment Enhancements

Potential later work:

- ARM64 + AMD64 multi-arch images.
- Signed container images.
- SBOM publishing.
- Read-only root filesystem.
- Distroless final image.
- Docker secrets support.

Kubernetes is explicitly out of scope for this project — see §15 Explicit Anti-Roadmap. Deployment enhancements MUST stay within the Docker Compose / bind-mount model.

The canonical simple deployment should remain:

```text
/opt/tiller-router/
├── docker-compose.yml
├── Dockerfile
├── .env
└── data/
```

with bind mounts and no named volumes.

---

# 15. Explicit Anti-Roadmap

The following should require strong justification before ever being added:

- Vector database platform.
- MCP marketplace.
- Prompt-management system.
- Agent framework.
- Conversation storage.
- Full observability platform.
- Enterprise policy engine.
- General-purpose workflow engine.
- Built-in secret manager.
- Distributed SQL datastore.
- Mandatory Redis.
- Mandatory PostgreSQL.
- Kubernetes support of any kind (manifests, Helm chart, or operation model). This project targets Docker Compose only.
- Complex billing system.

These are precisely the features that turn a small router into a large platform.

---

# 16. Recommended Roadmap Order

A sensible order after V1:

## Phase 1 — Harden
1. Provider credential encryption.
2. Provider health status.
3. Metadata-only request audit.
4. Scheduled backups.
5. Additional provider compatibility fixes.

## Phase 2 — Improve routing
1. Ordered fallback targets.
2. Retry policy.
3. Capability validation.
4. Compatibility aliases.

## Phase 3 — Improve operations
1. Usage statistics.
2. Dashboard.
3. Graceful client-key rotation.
4. Permission templates.
5. Catalogue-diff history.

## Phase 4 — Only if genuinely needed
1. Rate limits and quotas.
2. Weighted routing.
3. Rules-based routing.
4. SSO/RBAC.

---

# 17. Roadmap Guardrail

Before implementing any roadmap feature, confirm:

1. V1 remains stable.
2. The feature solves an observed problem.
3. It does not require an unnecessary external service.
4. It does not weaken the simple `/opt/tiller-router` Docker deployment.
5. It does not make model-selection semantics harder to understand.
6. It does not silently change deterministic routing behavior.
7. It does not introduce prompt/response storage by default.

If the feature fails those tests, it should probably live somewhere other than Tiller Router.
