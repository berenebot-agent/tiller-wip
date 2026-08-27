# Tiller Router — V1 Implementation Specification

**Status:** Frozen MVP specification  
**Project name:** Tiller Router  
**Runtime project path:** `/opt/tiller-router`  
**Primary implementation target:** Single Dockerized service with embedded SQLite persistence and HTTP admin UI  
**Purpose:** Provide a small, self-hosted LLM model-selection proxy that gives each client key a controlled model catalogue and can transparently route virtual models to arbitrary upstream provider/model targets without requiring client reconfiguration.

---

## 1. V1 Product Goal

Tiller Router exists to solve one narrow problem well:

> A client should keep one endpoint, one client key, and a stable model name while an administrator centrally controls which models that client can see and where those models actually route.

Tiller Router is **not** intended to be a full AI gateway platform in V1.

The core runtime flow is:

```text
Client
  |
  | client API key
  | requested model
  v
Tiller Router
  |
  +-- authenticate client key
  +-- determine models visible to that key
  +-- reject disabled/unknown models
  +-- resolve real or virtual model
  +-- select upstream provider instance
  +-- rewrite model name when required
  +-- forward request
  +-- stream response back
  v
Upstream LLM provider
```

---

## 2. Required Deployment Model

Tiller Router MUST be delivered as a Docker project.

The intended installation layout is:

```text
/opt/tiller-router/
├── docker-compose.yml
├── Dockerfile
├── .env
└── data/
```

Additional source-code files and build directories may exist in the project repository, but the runtime deployment MUST support the above layout.

### 2.1 Persistence rules

All persistent runtime state MUST live under:

```text
/opt/tiller-router/data/
```

The implementation MUST NOT use Docker named volumes.

The implementation MUST use bind mounts only.

The minimum persistent state is expected to include:

```text
/opt/tiller-router/data/
└── tiller-router.db
```

If additional persistent files are introduced, they MUST also live under `./data`.

Examples:

```text
data/
├── tiller-router.db
├── backups/
└── certificates/       # only if ever required later
```

V1 MUST NOT require PostgreSQL, Redis, ClickHouse, object storage, or any other external persistence service.

### 2.2 Docker Compose requirements

`docker-compose.yml` MUST:

- Define a single Tiller Router application service.
- Build from the included `Dockerfile`, or use an explicitly versioned published image once CI exists.
- Bind-mount `./data` into the container for all persistence.
- Define no Docker named volumes.
- Support environment-based admin credentials.
- Support connection to an existing external reverse-proxy Docker network.
- Avoid publishing the application port to the host when an external Docker proxy network is used.
- Support `restart: unless-stopped`.
- Include a health check.
- Run without privileged mode.
- Run without Docker socket access.

The deployment MUST be suitable for placement behind Nginx Proxy Manager or another reverse proxy.

### 2.3 Environment variables

At minimum:

```text
TILLER_ADMIN_USERNAME
TILLER_ADMIN_PASSWORD
TILLER_DATA_DIR
TILLER_LISTEN_ADDR
```

Recommended defaults:

```text
TILLER_DATA_DIR=/data
TILLER_LISTEN_ADDR=:8080
```

Admin credentials MUST NOT be hard-coded into the image.

---

## 3. Functional Scope

V1 consists of six core capabilities:

1. Provider management and model discovery.
2. Client API key management.
3. Per-client real-model permissions.
4. Virtual provider groups and virtual models.
5. Transparent request routing and model rewriting.
6. HTTP administration UI and REST management API.

---

# 4. Provider Management

## 4.1 Provider instances

An administrator MUST be able to create multiple independent provider instances.

A provider instance has:

- Unique administrator-defined name.
- Provider type.
- Base URL where applicable.
- API credential or credential reference.
- Model-discovery state.
- Default model-enable policy used when new models are discovered for each client key.
- Enabled/disabled operational state.
- Creation timestamp.
- Last successful model refresh timestamp.
- Last model refresh error, if any.

Provider instance names MUST be globally unique.

Examples:

```text
openrouter-personal
openrouter-work
openai-main
anthropic-main
ollama-hermes
ollama-gpu
deepseek
zai
```

The provider instance name is also the client-facing provider namespace for directly exposed real models.

Examples:

```text
openrouter-personal/anthropic/claude-sonnet-4
openai-main/gpt-5
ollama-hermes/qwen3:30b
```

Nested upstream model IDs are valid.

For example, an OpenRouter model ID:

```text
anthropic/claude-sonnet-4
```

becomes:

```text
openrouter-personal/anthropic/claude-sonnet-4
```

when exposed directly by Tiller Router.

---

## 4.2 Required provider types

V1 SHOULD support a practical list of common provider types.

Minimum target set:

- OpenAI.
- OpenAI Responses/Codex-compatible usage.
- Anthropic.
- OpenRouter.
- Ollama local.
- Ollama Cloud if API-compatible and available.
- DeepSeek.
- GLM / Z.ai.
- Generic OpenAI-compatible provider.

The generic OpenAI-compatible provider is important and MUST support:

- Administrator-defined base URL.
- Administrator-defined API key.
- Standard OpenAI-compatible `/v1/models`.
- Standard OpenAI-compatible chat/completions where available.
- Standard OpenAI-compatible Responses API where available.

Provider-specific adapters MAY be used where native APIs differ.

---

## 4.3 Provider credentials

Provider API credentials are administrator secrets.

Rules:

- Credential entry MUST occur only through the authenticated admin UI or management API.
- Credentials MUST never be returned by the client-facing API.
- Credentials MUST not be re-displayed in plaintext after entry.
- The admin UI MUST show only a masked/placeholder state such as `Configured`.
- The admin MUST be able to replace the credential.
- V1 may rely on host filesystem permissions and database file permissions for at-rest protection.
- Provider-credential encryption at rest is deferred to the roadmap.

---

# 5. Model Discovery

## 5.1 Initial discovery

When a provider instance is added, Tiller Router MUST attempt to fetch its available model catalogue immediately.

The discovered model catalogue MUST be stored in SQLite.

Client `/v1/models` requests MUST be served from Tiller Router's stored catalogue and permission state, not by live fan-out to upstream providers.

---

## 5.2 Scheduled refresh

Tiller Router MUST automatically refresh each provider's model catalogue approximately every 24 hours.

The exact scheduling implementation is flexible, but:

- Refresh MUST not require a container restart.
- Refresh failure MUST NOT delete the existing catalogue.
- The last successful catalogue MUST remain available.
- The admin UI MUST display last refresh status.
- The admin UI MUST provide a manual `Refresh Models` action.

---

## 5.3 Newly discovered models

Each client key has a per-provider-group setting controlling the default permission for future models.

Suggested UI label:

> **Auto-enable new models**

or:

> **New models default**

This setting is a **feeder/default only**.

It MUST NOT override any existing model permission.

Example:

```text
Provider group: openrouter-personal
New models default: OFF

Existing:
  model-a    ON
  model-b    OFF
  model-c    ON
```

If `model-d` is discovered tomorrow:

```text
model-d    OFF
```

Existing model permissions remain unchanged.

If the feeder is ON, newly discovered models start enabled for that client key.

---

## 5.4 Disappeared upstream models

If a model was previously discovered but later disappears from the upstream provider catalogue:

- DO NOT delete it automatically.
- Mark it unavailable/retired.
- Preserve all existing client permission records.
- Preserve virtual-model mappings referencing it.
- Surface a clear warning in the admin UI.
- Direct or virtual requests resolving to that unavailable model MUST fail clearly.
- The router MUST NOT silently choose another model.

---

# 6. Client API Keys

## 6.1 Client key properties

An administrator MUST be able to create named client keys.

Each client key has:

- Unique internal ID.
- Human-readable name.
- Optional description.
- Secret API key.
- Enabled/disabled state.
- Creation timestamp.
- Last rotation timestamp.
- Permission settings for real provider groups/models.
- Permission settings for virtual provider groups/models.

Examples:

```text
OpenCode - Hermes
Treasurer Agent
Hermes Server 3
Codex - Build Host
```

---

## 6.2 Secret storage

Client API keys MUST be **hash-only** after creation.

The hash algorithm MUST be a memory-hard KDF (argon2id preferred; bcrypt or scrypt are acceptable alternatives). A plain fast hash (e.g. unsalted SHA-256) MUST NOT be used, since these are bearer credentials with real upstream spend behind them. Roadmap §6.1 hash-hardening options (key prefixes, multiple valid hashes during rotation) build on this V1 baseline rather than replacing it.

Required behavior:

```text
Create key
  -> generate secret
  -> show plaintext once
  -> store only a secure hash
  -> never re-display original secret
```

The UI MAY retain and display a short non-secret fingerprint or suffix for identification.

Example:

```text
sk-tr-************************7f3a
```

Plaintext keys MUST NOT be recoverable from the SQLite database.

---

## 6.3 Key rotation

Each client key MUST have an immediate `Rotate` action.

Rotation behavior:

1. Generate a new random client secret.
2. Replace the stored hash.
3. Show the new plaintext secret once.
4. Immediately invalidate the old secret.
5. Preserve all key name, description, and permission state.

No overlap/grace period is required in V1.

---

## 6.4 Other key actions

Admin MUST be able to:

- Create.
- Rename.
- Edit description.
- Disable.
- Re-enable.
- Rotate.
- Delete.

Deleting a client key MUST immediately invalidate it.

---

# 7. Permission Model

## 7.1 Fundamental rule

The client key is the authority for what the client can directly request and what appears in its model catalogue.

A client MUST NOT be able to use a real or virtual model that is disabled for that key, even if the client knows or guesses the identifier.

---

## 7.2 Model permissions are authoritative

Permissions are stored at model level.

Each client key has an enabled/disabled permission for every relevant model.

Provider-group controls do NOT override existing model settings.

Provider-group controls act only as the default for newly discovered models.

---

## 7.3 Provider-group feeder setting

For every client key and every provider group:

```text
new_models_default = ON | OFF
```

This setting determines only the initial permission assigned to models first discovered after the setting exists.

It MUST NOT bulk-modify existing model permissions.

The UI MUST clearly explain this.

Recommended tooltip:

> Controls whether models discovered in future are enabled for this client. Changing this setting does not alter existing model permissions.

---

## 7.4 Disabled model behavior

If a client requests a model not enabled for its key:

- The request MUST be rejected.
- The router MUST NOT attempt upstream routing.
- The error SHOULD avoid disclosing hidden model details unnecessarily.

Recommended response class:

```text
404 model not found
```

or another consistent not-authorized/not-visible error.

---

# 8. Virtual Provider Groups and Virtual Models

## 8.1 Virtual provider groups

Tiller Router MUST support administrator-created **virtual provider groups**.

Examples:

```text
main
agents
coding
production
```

Virtual provider-group names MUST share the same global namespace as real provider-instance names.

Therefore these cannot coexist:

```text
real provider instance: main
virtual provider group: main
```

Provider-group names MUST be globally unique.

---

## 8.2 Virtual models

Each virtual provider group contains one or more virtual models.

Examples:

```text
main/general
main/coding
agents/hermes
agents/treasurer
```

The complete client-facing model identifier MUST be globally unique.

A virtual model maps to exactly one target in V1:

```text
virtual provider group
+ virtual model
  -> real provider instance
  -> real upstream model
```

Example:

```text
main/coding
  -> openrouter-personal
  -> anthropic/claude-sonnet-4
```

---

## 8.3 Virtual permissions

Virtual models have independent per-client permissions.

A virtual model MAY be enabled for a client even when the underlying real provider and underlying real model are hidden from that client.

Example:

```text
Client: OpenCode - Hermes

openrouter-personal/*     HIDDEN
main/coding               ENABLED
```

The client sees:

```text
main/coding
```

The client does not see:

```text
openrouter-personal/anthropic/claude-sonnet-4
```

but a request for:

```text
main/coding
```

MUST still route successfully to the hidden underlying target.

This is a core V1 requirement.

---

## 8.4 Virtual provider-group feeder setting

Virtual provider groups use the same client permission UX as real provider groups.

For each client key and virtual provider group:

```text
new_models_default = ON | OFF
```

This only affects virtual models created in that group in future.

It does not alter existing virtual-model permissions.

---

## 8.5 Immediate remapping

Changing a virtual model target MUST take effect for new requests immediately.

Example:

```text
Before:
main/coding
  -> openrouter-personal/anthropic/claude-sonnet-4
```

Administrator changes mapping:

```text
After:
main/coding
  -> openai-main/gpt-5
```

Requirements:

- No container restart.
- No client endpoint change.
- No client key change.
- No client model-name change.
- New requests use the new target immediately.
- Requests already in flight continue using the target resolved at request start.

---

## 8.6 Broken virtual mappings

If a virtual target is unavailable, retired, or otherwise invalid:

- Keep the virtual model configured.
- Display an admin warning.
- Do not silently redirect.
- Requests MUST fail clearly.

Recommended behavior:

```text
503 virtual model target unavailable
```

---

# 9. Client-Facing API

Tiller Router MUST expose a stable client-facing HTTP API.

## 9.1 Authentication

Client requests authenticate using the generated client API key.

The preferred standard form is:

```http
Authorization: Bearer <client-key>
```

Provider credentials MUST never be accepted from or exposed to clients.

---

## 9.2 Required endpoints

V1 MUST support:

```text
GET  /v1/models
POST /v1/chat/completions
POST /v1/responses
POST /v1/messages
```

`/v1/messages` is the Anthropic Messages API compatibility surface.

Additional endpoint aliases MAY be supported if required by provider/client compatibility.

---

## 9.3 `/v1/models`

`GET /v1/models` MUST:

- Authenticate the client key.
- Return only models enabled for that client.
- Include enabled direct real models.
- Include enabled virtual models.
- Exclude hidden provider groups/models.
- Exclude disabled models.
- Use canonical client-facing model IDs.

Examples:

```text
openai-main/gpt-5
ollama-hermes/qwen3:30b
main/coding
agents/hermes
```

Temporary provider outages MUST NOT cause models to disappear from `/v1/models`.

Unavailable/retired models SHOULD NOT be presented as healthy/usable. The exact V1 presentation may either omit them or include explicit availability metadata if compatible with clients; consistency is required.

---

# 10. Request Routing

## 10.1 Direct real model request

Example client request:

```text
model = openai-main/gpt-5
```

Routing:

1. Authenticate key.
2. Confirm `openai-main/gpt-5` is enabled for that key.
3. Resolve provider instance `openai-main`.
4. Rewrite the outgoing upstream model ID to the provider-native model ID `gpt-5`.
5. Apply provider credential.
6. Forward request.
7. Stream/pass response back.

---

## 10.2 Virtual model request

Example:

```text
model = main/coding
```

Routing:

1. Authenticate key.
2. Confirm `main/coding` is enabled for that key.
3. Resolve virtual mapping.
4. Ignore whether the underlying real provider/model is directly visible to that key.
5. Select mapped provider instance.
6. Rewrite outgoing model field to mapped provider-native model ID.
7. Apply upstream credential.
8. Forward request.
9. Return response to client while preserving the virtual identity where appropriate.

---

## 10.3 Response model identity

Where the upstream response includes a model identifier, Tiller Router SHOULD rewrite that identifier back to the client-requested canonical model name when doing so is protocol-safe.

Example:

```text
Client requested: main/coding
Upstream used:    anthropic/claude-sonnet-4
Client receives:  main/coding
```

The underlying provider/model SHOULD remain transparent to the client for virtual routes.

---

# 11. Streaming and Protocol Fidelity

## 11.1 General rule

Tiller Router MUST stream upstream responses without buffering the full response body.

The router MAY buffer the inbound request body only as required to inspect and rewrite routing fields.

---

## 11.2 Supported behavior

V1 MUST support:

- Streaming Chat Completions.
- Non-streaming Chat Completions.
- Streaming Responses API.
- Non-streaming Responses API.
- Anthropic Messages API.
- Tool/function calls.
- Tool IDs and arguments.
- Finish reasons.
- Reasoning/thinking fields where supported.
- Client cancellation/disconnect propagation.
- Upstream rate-limit responses.
- Upstream timeouts.
- Upstream HTTP errors.

---

## 11.3 Translation

Where the selected upstream provider does not natively use the client-facing protocol, Tiller Router MAY translate between supported request/response formats.

Translation MUST preserve:

- Message roles and order.
- Tool call IDs.
- JSON tool arguments.
- Tool results.
- Streaming event ordering.
- Finish/stop semantics.
- Reasoning/thinking fields where technically representable.
- Error status where possible.

Protocol translation is part of V1 because the intended clients include OpenAI/Codex-style and Anthropic-style agents.

---

# 12. Provider Adapter Contract

Each provider adapter SHOULD expose a common internal interface approximately equivalent to:

```text
ValidateConfiguration()
DiscoverModels()
PrepareChatCompletionRequest()
PrepareResponsesRequest()
PrepareMessagesRequest()
SendRequest()
NormalizeError()
```

The exact programming-language interface is implementation-defined.

Every provider adapter MUST define:

- Provider type identifier.
- Default base URL, if any.
- Required credential form.
- Model discovery strategy.
- Supported client-facing protocol surfaces.
- Provider-native model ID mapping rules.
- Required request headers.
- Error normalization strategy.

Generic OpenAI-compatible providers MUST use the same adapter wherever possible.

---

# 13. Admin REST API

The web UI MUST use Tiller Router's management API rather than directly manipulating the database.

The exact URL design may vary, but V1 MUST provide equivalent capabilities.

Recommended API groups:

```text
/api/admin/session
/api/admin/providers
/api/admin/providers/{id}
/api/admin/providers/{id}/refresh
/api/admin/providers/{id}/models

/api/admin/virtual-groups
/api/admin/virtual-groups/{id}
/api/admin/virtual-models
/api/admin/virtual-models/{id}

/api/admin/client-keys
/api/admin/client-keys/{id}
/api/admin/client-keys/{id}/rotate
/api/admin/client-keys/{id}/permissions

/api/admin/backup/export
/api/admin/health
```

API mutation endpoints MUST require authenticated administrator access.

---

# 14. Admin Authentication

V1 uses one administrator account.

Credentials are supplied via Docker Compose/environment:

```text
TILLER_ADMIN_USERNAME
TILLER_ADMIN_PASSWORD
```

Requirements:

- Admin UI requires authentication.
- Admin REST API requires authentication.
- Session cookies MUST be HTTP-only.
- Secure cookies SHOULD be enabled when HTTPS is detected/configured.
- Password MUST NOT be written to logs.
- No multi-user RBAC is required in V1.

---

# 15. Admin UI

V1 MUST include an HTTP admin UI.

The UI SHOULD remain intentionally small.

Required primary sections:

1. Providers.
2. Real Models.
3. Virtual Providers / Models.
4. Client Keys.
5. Backup / System.

---

## 15.1 Providers screen

Must support:

- Add provider instance.
- Choose provider type.
- Set unique provider-instance name.
- Set base URL when applicable.
- Enter/replace API credential.
- View discovery state.
- Manual model refresh.
- View last successful refresh.
- View discovery errors.
- Delete provider when safe.

Deletion MUST be blocked if a virtual model still references the provider.

---

## 15.2 Models screen

For each provider:

- List discovered models.
- Show available/retired state.
- Search/filter models.
- Show provider-native model ID.
- Show client-facing canonical model ID.

V1 does not require global model enable/disable because permissions are client-key specific.

---

## 15.3 Virtual Providers / Models screen

Must support:

- Create virtual provider group.
- Rename virtual provider group.
- Delete virtual provider group if empty/safe.
- Create virtual model.
- Set virtual model name.
- Select one provider instance.
- Select one target model.
- Change mapping immediately.
- Show invalid/unavailable mapping warning.
- Delete virtual model.

Renaming provider groups or model IDs is a breaking client-facing change.

The UI MUST warn before such renames.

---

## 15.4 Client Keys screen

Must support:

- Create key.
- Show plaintext key exactly once.
- Rename key.
- Edit description.
- Disable/re-enable.
- Rotate.
- Delete.
- Configure permissions.

Permission UI SHOULD group models by provider group.

For each real or virtual provider group show:

```text
New models default: ON/OFF
```

Below it, show individual model toggles.

The UI MUST state clearly:

> The provider-group setting only controls the default for models added in future. It does not override existing model permissions.

---

# 16. Data Model

The exact schema is implementation-defined, but the following entities are required.

## 16.1 providers

Suggested fields:

```text
id
name
type
base_url
credential_secret
enabled
last_refresh_at
last_refresh_error
created_at
updated_at
```

`name` is globally unique across real and virtual provider-group namespaces.

---

## 16.2 provider_models

Suggested fields:

```text
id
provider_id
upstream_model_id
canonical_model_id
display_name
available
first_seen_at
last_seen_at
created_at
updated_at
```

Unique constraint:

```text
(provider_id, upstream_model_id)
```

---

## 16.3 virtual_provider_groups

Suggested fields:

```text
id
name
created_at
updated_at
```

`name` must be globally unique across both real provider instances and virtual groups.

---

## 16.4 virtual_models

Suggested fields:

```text
id
virtual_group_id
name
target_provider_id
target_provider_model_id
created_at
updated_at
```

Unique constraint:

```text
(virtual_group_id, name)
```

The complete client-facing ID must also be globally collision-free.

---

## 16.5 client_keys

Suggested fields:

```text
id
name
description
secret_hash
secret_fingerprint
enabled
created_at
rotated_at
updated_at
```

Plaintext client secrets MUST NOT be stored.

---

## 16.6 client_group_defaults

Stores the feeder/default for new models.

Suggested fields:

```text
client_key_id
group_kind        # real | virtual
group_id
new_models_enabled
updated_at
```

Unique constraint:

```text
(client_key_id, group_kind, group_id)
```

---

## 16.7 client_model_permissions

Suggested fields:

```text
client_key_id
model_kind        # real | virtual
model_id
enabled
created_at
updated_at
```

Unique constraint:

```text
(client_key_id, model_kind, model_id)
```

---

# 17. SQLite and Persistence

SQLite is the sole V1 persistence engine.

Requirements:

- Database lives under `/data` in the container.
- Host bind mount maps `/opt/tiller-router/data` to container `/data`.
- Use WAL mode where appropriate.
- Use foreign keys.
- Database schema migrations MUST run automatically at startup.
- Migration failure MUST stop startup safely.
- The database MUST not be silently reset or recreated over an incompatible/corrupt DB.

All application state required to restore operation MUST live in SQLite, except:

- Environment-supplied administrator credentials.
- Build/runtime configuration that is intentionally supplied through environment variables.

---

# 18. Backup / Export

V1 MUST provide a simple administrator-triggered export.

Preferred implementation:

- Produce a consistent SQLite backup file using SQLite's backup API or equivalent safe mechanism.
- Do not copy a live WAL database unsafely.
- Provide download through the authenticated admin API/UI.
- Optionally also write backups under:

```text
/data/backups/
```

The export naturally includes:

- Providers.
- Provider model catalogues.
- Provider credential state as stored by V1.
- Virtual provider groups.
- Virtual mappings.
- Client-key metadata.
- Client-key hashes.
- Permissions.
- Catalogue state.

Because client keys are hash-only, exported backups MUST NOT make existing client secrets recoverable.

A restored database preserves valid client-key authentication because the hashes are preserved.

**Provider credential exposure in exports.** Provider-credential encryption at rest is deferred to the roadmap (§4.3, §23). Until it exists, provider credentials are stored in a recoverable form and are therefore included in backup exports in that same recoverable form — unlike client keys, they are NOT reduced to a non-recoverable hash. A V1 backup file MUST be treated as a secret on par with the provider credentials themselves. The admin UI/API documentation MUST state this plainly (e.g. "This backup file contains recoverable provider API credentials; store and transmit it accordingly") wherever the export/download action is presented, so this isn't a silent surprise to the administrator.

---

# 19. Provider Deletion Rules

Provider deletion MUST be blocked if:

- Any virtual model references the provider.
- Other database constraints make deletion unsafe.

The UI/API MUST report the dependency clearly.

The administrator must first:

- Repoint dependent virtual models, or
- Delete those virtual models.

No automatic cascading deletion of virtual mappings.

---

# 20. Rename Rules

Provider-instance names, virtual provider-group names, and virtual model names MAY be renamed.

However:

- Renaming changes client-facing model IDs.
- Renaming is therefore a breaking change.
- Admin UI MUST warn before rename.
- Internal references MUST be updated transactionally.
- Existing client permission records MUST remain attached to the renamed entity.

No automatic compatibility alias is required in V1.

---

# 21. Failure Behavior

## 21.1 Provider outage

If an upstream provider is unreachable:

- Do not remove models from the client catalogue.
- Do not reroute.
- Do not fall back.
- Return the upstream failure or a clear gateway failure.

Fallback routing is not part of V1.

---

## 21.2 Invalid client key

Return:

```text
401 Unauthorized
```

---

## 21.3 Disabled client key

Return:

```text
401 Unauthorized
```

V1 standardizes on `401` for both an invalid key (§21.2) and a disabled key, so clients see one consistent "not authenticated" class rather than having to distinguish the two. Do not use `403` for this case.

---

## 21.4 Hidden/disabled model

Return a consistent not-visible/not-authorized response.

Preferred:

```text
404 model not found
```

This avoids exposing hidden catalogue information.

---

## 21.5 Broken virtual mapping

Return:

```text
503 Service Unavailable
```

with a concise machine-readable error.

---

# 22. Logging and Privacy

V1 MUST NOT log prompt or response bodies.

V1 SHOULD log only operational metadata needed for service operation, such as:

- Startup.
- Shutdown.
- Provider refresh success/failure.
- Authentication failures without secrets.
- Routing failures without prompts.
- Upstream status/error class.
- Admin mutations.

Do not log:

- Client API keys.
- Provider credentials.
- Authorization headers.
- Prompt content.
- Response content.
- Tool arguments.
- Reasoning content.

Usage analytics and detailed audit logging are deferred to the roadmap.

---

# 23. Security Requirements

V1 MUST:

- Require client authentication on inference endpoints.
- Require admin authentication on admin UI/API.
- Hash client API keys.
- Never re-display provider credentials.
- Avoid prompt/response logging.
- Run unprivileged where practical.
- Require no Docker socket.
- Require no privileged container mode.
- Support reverse-proxy-only exposure.
- Persist only under `/data`.
- Avoid named Docker volumes.
- Use safe SQL parameterization.
- Protect against obvious CSRF/session issues in admin UI.
- Validate provider URLs before use.
- Set reasonable upstream request timeouts.
- Propagate client cancellation.

Provider credential encryption at rest is deferred.

---

# 24. Health Endpoints

The service SHOULD expose:

```text
GET /health/live
GET /health/ready
```

`live` means process is running.

`ready` means:

- Database opened.
- Migrations succeeded.
- Router initialized.

Readiness MUST NOT depend on every upstream provider being reachable.

---

# 25. Docker Image Requirements

The `Dockerfile` SHOULD use a multi-stage build.

Preferred properties:

- Small final image.
- Non-root runtime user.
- No compiler/toolchain in final image.
- Static or minimally dependent binary where practical.
- Embedded web UI assets where practical.
- Healthcheck-compatible runtime.
- Explicit working directory.
- `/data` created and writable by runtime UID.

The runtime MUST not require package installation at container start.

---

# 26. Suggested Repository Structure

Implementation language is not mandated by this specification, but Go is a strong fit.

Suggested project layout:

```text
tiller-router/
├── Dockerfile
├── docker-compose.yml
├── .env.example
├── README.md
├── go.mod
├── go.sum
├── cmd/
│   └── tiller-router/
├── internal/
│   ├── admin/
│   ├── auth/
│   ├── config/
│   ├── database/
│   ├── models/
│   ├── providers/
│   ├── proxy/
│   ├── routing/
│   └── web/
├── migrations/
├── web/
├── tests/
└── data/
    └── .gitkeep
```

The committed `data/` directory MUST NOT contain real runtime data.

---

# 27. V1 Non-Goals

The coding agent MUST NOT add these unless needed strictly to satisfy a V1 requirement:

- Fallback routing.
- Weighted routing.
- Load balancing.
- Cost optimization.
- Token accounting.
- Request history.
- Prompt logging.
- Response logging.
- Analytics dashboard.
- Per-key budgets.
- Per-key quotas.
- Rate limiting.
- Semantic caching.
- MCP.
- Vector databases.
- Embedding storage.
- Guardrails platform.
- SSO.
- Multi-admin RBAC.
- Kubernetes-specific control plane.
- Redis.
- PostgreSQL.
- ClickHouse.
- Clustering.
- Distributed state.
- Automatic "best model" selection.
- Provider health-based automatic failover.
- Compatibility aliases for renamed models.
- Re-viewable client secrets.
- Separate external database services.

If implementation work starts to require these features, stop and reconsider scope.

---

# 28. V1 Acceptance Tests

V1 is complete only when all critical acceptance tests pass.

## 28.1 Deployment

- Fresh clone/build succeeds.
- `docker compose up -d` starts one application container.
- No external database container exists.
- No Docker named volume exists.
- All persistent state is under `./data`.
- Restart preserves all configuration.
- Reverse proxy can reach the application over the configured Docker network.
- Application does not require a host-published port.

---

## 28.2 Provider discovery

- Add an OpenAI-compatible provider.
- Credential is accepted but cannot be re-viewed.
- Models are fetched immediately.
- Manual refresh works.
- Scheduled refresh is configured.
- Refresh failure preserves existing catalogue.
- Newly discovered model follows the client/provider `new models default`.
- Previously configured model permissions remain unchanged.

---

## 28.3 Client catalogue isolation

Create two client keys with different permissions.

Verify:

- `/v1/models` differs by client key.
- Each key sees only enabled models.
- Hidden models cannot be called directly by guessing the ID.
- Disabled key cannot authenticate.
- Rotated key invalidates old secret immediately.

---

## 28.4 Virtual routing

Configure:

```text
main/coding
  -> provider-a/model-a
```

For a client:

```text
provider-a/model-a    DISABLED
main/coding           ENABLED
```

Verify:

- `/v1/models` shows `main/coding`.
- `/v1/models` does not show `provider-a/model-a`.
- Request to `main/coding` reaches `provider-a/model-a`.
- Direct request to `provider-a/model-a` is rejected.
- Response identity remains `main/coding` where protocol-safe.

This test is mandatory.

---

## 28.5 Immediate remap

With client configuration unchanged:

1. Request `main/coding`.
2. Verify it reaches provider/model A.
3. Admin changes mapping to provider/model B.
4. Do not restart.
5. Do not change client key.
6. Do not change endpoint.
7. Do not change client model.
8. Request `main/coding` again.
9. Verify it reaches provider/model B.

This is the primary product acceptance test.

---

## 28.6 Streaming

Verify:

- Chat Completions streaming begins before full upstream completion.
- Responses API streaming works.
- Anthropic Messages streaming works.
- Proxy does not buffer entire response.
- Client disconnect cancels/abandons upstream request appropriately.
- Tool-call streaming remains valid.

---

## 28.7 Broken mappings

- Retire/remove target model from refreshed provider catalogue.
- Virtual mapping remains configured.
- Admin UI warns.
- Request returns clear failure.
- No fallback occurs.

---

## 28.8 Provider outage

- Make provider unreachable.
- Models remain configured.
- Requests fail.
- No fallback occurs.
- Provider outage does not corrupt catalogue or permissions.

---

## 28.9 Backup

- Export backup.
- Stop service.
- Restore exported DB into a clean `./data`.
- Start service.
- Providers, models, virtual mappings, client metadata and permissions are restored.
- Existing client keys still authenticate.
- No plaintext client key can be extracted from backup.

---

# 29. Definition of Done

Tiller Router V1 is done when:

- The Docker deployment requirements are satisfied.
- The full V1 API surfaces work.
- Provider discovery works.
- Client-specific model visibility works.
- Virtual provider groups/models work.
- Hidden underlying targets remain routable through authorized virtual models.
- Immediate central remapping works.
- Streaming works.
- Admin UI supports all required CRUD and permission operations.
- SQLite persistence and backup work.
- All critical acceptance tests pass.
- No roadmap feature has been allowed to materially expand V1 scope.

