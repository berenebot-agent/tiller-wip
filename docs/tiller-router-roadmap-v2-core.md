# Tiller Router — Core Product Roadmap v2

**Project:** Tiller Router  
**Status:** Active roadmap  
**Scope:** Core router/product functionality only  
**Explicit exclusion:** Multi-user, workspace, signup and SaaS tenancy features are NOT part of this roadmap. They are documented separately in `tiller-router-roadmap-saas-multiuser.md`.

> This is the comprehensive core roadmap. It supersedes the original
> `tiller-router-roadmap.md`, whose deferred core items are folded into the
> Deferred Backlog (§23) and whose guardrails are restated in §24 and §25.

---

# 1. Product Goal

Tiller Router should first become an excellent single-instance model-selection and routing control plane.

The target product experience is:

```text
Client
  |
  | fixed endpoint
  | fixed client key
  | stable model name
  v
Tiller Router
  |
  +-- expose only approved real + virtual models
  +-- route real models directly
  +-- route virtual models transparently
  +-- change virtual targets centrally
  +-- preserve streaming + tool/protocol fidelity
  +-- explain what happened operationally
  +-- survive expected provider failures predictably
  v
Upstream provider/model
```

The core milestone is:

> Point all of our agents and coding tools at Tiller, expose exactly the model catalogue we want, centrally change backend models, survive provider issues in a controlled way, and understand what Tiller routed and why.

---

# 2. Product Boundary

Tiller Router should remain small.

It is primarily:

- provider/model catalogue management;
- per-client model visibility;
- virtual model abstraction;
- protocol-compatible routing;
- routing reliability;
- operational visibility;
- simple self-hosted administration.

It is NOT yet:

- a multi-user SaaS;
- a billing platform;
- an agent framework;
- a workflow engine;
- a prompt-management product;
- a vector/MCP platform;
- a general AI observability stack.

---

# 3. Immediate Roadmap Order

Recommended implementation sequence:

## Phase A — Metadata Activity Logging

Implement the already scoped metadata-only request logging feature.

This is the next feature.

Goals:

- one metadata row per routed request;
- no prompt/response persistence;
- per-client logging enable/disable;
- per-client retention;
- global defaults copied at client-key creation;
- per-client Activity UI;
- token counts where safely obtainable;
- request latency/status;
- resolved provider/model;
- hourly pruning;
- SQLite only.

This gives visibility before more complicated routing logic is added.

---

## Phase B — Provider Health + Catalogue Lifecycle

Add operational understanding of providers and their changing catalogues.

Goals:

- provider health state;
- clear auth/unreachable/rate-limit state;
- model appeared/disappeared/reappeared history;
- retired/unavailable model warnings;
- virtual target warnings;
- health remains informational initially.

Do NOT automatically reroute yet.

---

## Phase C — Fallback Routing

Add deterministic ordered fallbacks to virtual models.

Example:

```text
main/coding
  1. openrouter/anthropic/claude-...
  2. openai/gpt-...
  3. ollama-local/qwen-...
```

Fallback semantics must be explicit and testable.

---

## Phase D — Retry Policy

Add conservative retries only after fallback behaviour is well-defined.

Retries must not create duplicate side effects or duplicate tool execution.

---

## Phase E — Model Capability Metadata + Compatibility

Improve admin confidence when remapping virtual models.

Track and/or infer useful capability metadata:

- tool calling;
- Chat Completions;
- Responses API;
- Anthropic Messages;
- vision;
- reasoning/thinking;
- context window;
- streaming support.

Use this to warn when a selected virtual target may not be compatible with the client-facing use case.

---

## Phase F — Client Compatibility Aliases

Allow optional aliases where clients require specific model names.

Example:

```text
canonical:
  main/coding

aliases:
  coding
  default
  claude-sonnet
```

Aliases must remain a controlled compatibility feature, not become a second routing language.

---

## Phase G — Hardening

After the router behaviour is proven:

- provider credential encryption at rest;
- safe scheduled SQLite backups;
- restore validation;
- graceful client-key rotation;
- stronger deployment hardening.

---

## Phase H — Operational Views / Usage

Once metadata logging is stable:

- workspace-free global activity view;
- requests by client key;
- requests by virtual model;
- requests by real provider/model;
- latency;
- errors;
- token counts;
- provider health history.

No body logging.

---

## Phase I — Real-World Burn-In

Use Tiller heavily with:

- OpenCode;
- Hermes agents;
- Codex-style clients;
- Anthropic-style clients;
- OpenAI-compatible tools.

Track actual friction before expanding scope further.

Only after this phase should the separate SaaS / multi-user roadmap be reconsidered.

---

# 4. Phase A — Metadata-Only Request Logging

This phase incorporates the already scoped logging feature.

## 4.1 Privacy boundary

Never persist:

- prompts;
- responses;
- tool arguments;
- tool results;
- reasoning/thinking content;
- Authorization headers;
- provider credentials;
- plaintext client keys.

Response bodies may be inspected transiently in memory only where needed to extract usage metadata.

---

## 4.2 Request log fields

Suggested:

```text
id
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
created_at
```

`resolved_provider` and `resolved_model` should be stored as names, not foreign IDs, so historical rows remain meaningful after rename/deletion.

---

## 4.3 What gets logged

Log every proxy request that reaches routing with:

- a valid client key;
- a requested model.

Include failures.

If routing resolution fails:

```text
resolved_provider = NULL
resolved_model = NULL
```

---

## 4.4 Write strategy

Use synchronous best-effort SQLite insert.

Requirements:

- logging failure never fails inference;
- no async queue initially;
- no Redis;
- no external service;
- WAL mode;
- keep implementation simple.

---

## 4.5 Per-client configuration

Each client key stores:

```text
logging_enabled
retention_days
```

New clients copy the current global defaults.

Later changes to defaults do not alter existing client-key settings.

---

## 4.6 Activity UI

Each client-key row gets:

```text
Activity
```

Activity view includes:

- timestamp;
- requested model;
- resolved provider/model;
- protocol;
- streaming state;
- HTTP status;
- latency;
- input/output tokens;
- provider request ID;
- search;
- pagination;
- clear logs.

No export initially.

---

## 4.7 Pruning

Run retention pruning:

- at startup;
- approximately hourly.

Deleting a client key deletes its logs.

---

# 5. Phase B — Provider Health

## 5.1 Proposed health states

```text
healthy
degraded
unreachable
auth_failed
rate_limited
unknown
```

Health is separate from model discovery.

A temporary outage must not delete models from the catalogue.

---

## 5.2 Initial behaviour

Provider health is informational only.

It should:

- show in the admin UI;
- show last successful request/health check;
- show recent failures;
- help explain routing failures.

It should NOT:

- automatically disable models;
- remove models from `/v1/models`;
- trigger fallback until fallback behaviour is implemented explicitly.

---

# 6. Phase B — Catalogue Lifecycle

Track model lifecycle events:

```text
discovered
disappeared
reappeared
retired/unavailable
```

Keep retired models so:

- existing permissions remain;
- virtual mappings remain;
- admin can see what broke.

Do not silently retarget virtual models.

---

# 7. Phase C — Fallback Routing

Fallbacks apply to **virtual models**, not direct real-model calls.

Example:

```text
main/coding
  primary:  openrouter/anthropic/claude...
  fallback: openai/gpt...
```

Direct call:

```text
openrouter/anthropic/claude...
```

should continue to call exactly that target and fail if it fails.

---

## 7.1 Candidate fallback triggers

Possible triggers:

- connection failure;
- DNS failure;
- provider timeout before response;
- HTTP 429;
- selected 5xx responses;
- provider auth failure;
- upstream model not found.

These MUST NOT be assumed automatically.

They need explicit product decisions before implementation.

---

## 7.2 Streaming rule

Once response bytes have been sent to the client, automatic fallback is generally unsafe.

Default principle:

> Fallback may occur only before client-visible response streaming has begun.

If a stream fails after output begins, return/terminate that failure rather than silently starting a second model response.

---

## 7.3 Fallback visibility

Metadata logging should record:

- requested virtual model;
- primary attempted target;
- final resolved target;
- fallback reason;
- attempt count.

This likely requires extending the Phase A log schema later.

---

# 8. Phase D — Retry Policy

Retries and fallbacks are different.

A retry means:

```text
same provider/model
try again
```

A fallback means:

```text
different configured target
```

Retries should be conservative.

Do not retry:

- after response streaming starts;
- requests where duplicate execution could cause external side effects;
- tool actions blindly.

---

# 9. Phase E — Capability Metadata

Provider/model catalogue records may gain:

```text
supports_chat_completions
supports_responses
supports_messages
supports_tools
supports_streaming
supports_vision
supports_reasoning
context_window
pricing_input
pricing_output
```

Values may be:

```text
yes
no
unknown
```

Unknown must remain a first-class state rather than pretending metadata is complete.

---

# 10. Compatibility Checking

When a virtual model target changes, Tiller can warn:

```text
Warning:
This target is not known to support Anthropic Messages.
Clients using /v1/messages may fail.
```

Initially warnings only.

Do not block administrators from making deliberate mappings unless a later strict-mode feature is explicitly added.

---

# 11. Phase F — Aliases

Aliases should be optional per model.

Example:

```text
main/coding
aliases:
  coding
  default
```

Rules:

- canonical model ID remains unique;
- aliases must be unique across visible model IDs;
- alias resolves to one canonical real/virtual model;
- `/v1/models` behaviour should be configurable or clearly defined;
- aliases must not create ambiguous routing.

---

# 12. Phase G — Provider Credential Encryption

Provider credentials are currently protected by filesystem/database access controls.

Future hardening:

- master encryption key outside SQLite;
- authenticated encryption;
- per-record nonce;
- provider credential remains non-viewable after entry;
- credential replacement supported;
- master-key rotation designed before coding.

Client API keys remain hash-only.

---

# 13. Phase G — Backups

Add scheduled safe SQLite backup.

Preferred path:

```text
/data/backups/
```

Possible retention:

```text
daily   x 7
weekly  x 4
monthly x 6
```

Use SQLite backup semantics, not unsafe raw copying of a live WAL database.

---

# 14. Graceful Client-Key Rotation

Current rotation:

```text
new key generated
old key dies immediately
```

Future option:

```text
Keep old key valid:
  0 minutes
  15 minutes
  1 hour
  24 hours
```

Useful for rolling keys across many deployed clients.

---

# 15. Operational Views

Potential views after logging matures:

## 15.1 Global Activity

Across all client keys:

- time;
- client;
- requested model;
- resolved target;
- status;
- latency;
- tokens.

## 15.2 Virtual Model View

For each virtual model:

- current mapping;
- recent route volume;
- error rate;
- fallback events;
- latency.

## 15.3 Provider View

- health;
- recent errors;
- request volume;
- discovered/retired models;
- catalogue refresh history.

Keep this operational rather than turning it into a large analytics product.

---

# 16. Permission UX Improvements

Possible later convenience features:

- enable all current models;
- disable all current models;
- filter + bulk toggle;
- copy permissions from another client;
- saved permission templates.

The established feeder behaviour remains:

> Provider/group `Auto-enable new models` affects only newly discovered/created models. It does not overwrite existing individual model settings.

---

# 17. Additional Provider Support

Add native adapters only when generic compatibility is insufficient.

Possible future providers:

- Gemini;
- Vertex;
- Bedrock;
- Azure OpenAI;
- Groq;
- Mistral;
- xAI;
- Together;
- Fireworks;
- Cerebras.

Prefer generic OpenAI-compatible configuration whenever it works.

---

# 18. Additional Protocol Surfaces

Core remains:

- OpenAI Chat Completions;
- OpenAI Responses;
- Anthropic Messages.

Potential later:

- embeddings;
- images;
- audio;
- realtime;
- batch;
- reranking.

Only add when there is an actual client requirement.

---

# 19. Deployment Hardening

Possible later improvements:

- multi-arch images;
- signed images;
- SBOM;
- smaller/distroless final image;
- read-only root filesystem;
- Docker secrets.

Canonical deployment should remain:

```text
/opt/tiller-router/
├── docker-compose.yml
├── Dockerfile
├── .env
└── data/
```

One application container.

SQLite.

Bind-mounted `./data`.

No required named volumes.

No mandatory external DB.

---

# 20. Explicit Non-Goals for Core Roadmap

Do not introduce yet:

- users table;
- workspaces;
- memberships;
- public signup;
- SaaS billing;
- organisation administration;
- OAuth/OIDC;
- tenant quotas;
- customer-facing hosted plans.

Those are documented separately.

Also avoid:

- vector database;
- MCP marketplace;
- prompt-management platform;
- agent framework;
- workflow engine;
- conversation storage;
- full observability platform;
- enterprise policy engine;
- built-in secret manager;
- distributed SQL datastore;
- distributed control plane;
- mandatory Redis/Postgres/ClickHouse;
- complex billing system.

These are precisely the features that turn a small router into a large
platform. They should require strong justification before ever being added.

---

# 21. Grill-Me Decision Gates

Before coding each major feature, answer the associated questions.

These are intentionally unresolved until needed.

---

## 21.1 Logging — remaining decisions

1. Should global logging default be ON or OFF on a clean install?
2. Is 30 days the right default retention?
3. Do we want `synchronous=NORMAL` for SQLite WAL now?
4. Should Activity show provider error text, or only HTTP status/error class?
5. Should request logs include the client-visible request ID if one exists?
6. Should token usage from fallbacks later aggregate attempts or record each attempt separately?

---

## 21.2 Provider health

1. Active polling, passive request-derived health, or both?
2. What polling interval?
3. Should health checks incur billable inference calls, or use only non-billable endpoints?
4. When is a provider `degraded` versus `unreachable`?
5. How long until a provider recovers to healthy?
6. Do auth failures require special admin prominence?
7. Should manual `Test Provider` exist?

Recommended starting bias:

> Passive health + explicit Test Provider first; only add scheduled active probing if needed.

---

## 21.3 Catalogue lifecycle

1. How long do retired models remain?
2. Can the admin manually purge retired models?
3. What happens if a retired model reappears?
4. Should previous client permissions automatically remain when it reappears?
5. Should catalogue change history have retention?
6. Do we notify visually when a virtual target disappears?

Recommended starting bias:

> Preserve indefinitely unless manually purged; restore prior permissions if the same provider/model ID reappears.

---

## 21.4 Fallbacks

1. How many fallback targets can one virtual model have?
2. Ordered only, or weighted too?
3. Which failures trigger fallback?
4. Does 429 trigger fallback immediately?
5. Which 5xx statuses?
6. Does timeout trigger fallback?
7. Does auth failure trigger fallback?
8. Does upstream `model_not_found` trigger fallback?
9. Maximum total attempts?
10. Maximum total wall-clock time?
11. What happens if streaming has already begun?
12. Should fallback events be visible to clients?
13. Should the response model still report the virtual model name?
14. Should the admin UI show primary/fallback list as drag-and-drop order?
15. Should direct real-model requests ever fall back?

Recommended starting bias:

- ordered targets only;
- no weighting;
- max 2 or 3 attempts;
- only pre-stream fallback;
- direct real models never fall back;
- client continues to see virtual model identity.

---

## 21.5 Retries

1. Which network failures can retry?
2. Maximum retries?
3. Backoff?
4. Can non-streaming inference be safely retried after upstream accepted the request but connection dropped?
5. How do tool calls affect retry safety?
6. Should retry happen before moving to next fallback target?

Recommended starting bias:

> Very conservative; connection establishment failures only at first.

---

## 21.6 Capabilities

1. Where does capability data come from?
2. Provider catalogue metadata?
3. Tiller-maintained known-model registry?
4. Runtime probing?
5. Manual admin override?
6. Should unknown be treated as supported or unsupported?
7. Should incompatibility be warning-only or blocking?
8. Do virtual models have declared expected capability profiles?

Recommended starting bias:

> Provider metadata + manual override, unknown allowed, warning-only.

---

## 21.7 Aliases

1. Should aliases appear in `/v1/models`?
2. Can one canonical model have many aliases?
3. Can alias names contain `/`?
4. Should an alias be global or per-client?
5. What happens if alias collides with a newly discovered real model?
6. Should aliases survive canonical rename?

Recommended starting bias:

> Global aliases, visible in `/v1/models`, collision prohibited.

---

## 21.8 Provider credential encryption

1. Environment master key or mounted key file?
2. What happens when encryption key is missing?
3. How is key rotation performed?
4. Should backups include encrypted credentials?
5. Should there be an export excluding credentials?

---

## 21.9 Backups

1. Automatic backups enabled by default?
2. Default retention?
3. Should backup include request activity logs?
4. Manual download only, or scheduled local backups too?
5. Do we want one-click restore through UI or CLI-only restore?
6. Should the router stop writes briefly during restore?

Recommended starting bias:

> Scheduled local backup + authenticated download; CLI restore initially.

---

## 21.10 Usage views

1. Do we care about cost calculation?
2. If yes, source of pricing?
3. Do fallback attempts count separately?
4. What time windows are useful?
5. Do we need CSV export?
6. Is this operational debugging or a billing-grade ledger?

Recommended starting bias:

> Operational only, not billing-grade.

---

# 22. Core Roadmap Definition of Success

Before considering the SaaS roadmap, Tiller should demonstrate:

- stable daily use;
- multiple real providers;
- multiple clients;
- virtual model remapping;
- clean model visibility controls;
- reliable streaming;
- tool-call fidelity;
- metadata Activity useful in practice;
- provider failures understandable;
- fallback behaviour predictable;
- no recurring need to edit client-side model configuration;
- low operational burden;
- simple Docker deployment remains intact.

If those are true, Tiller has earned the complexity of becoming a hosted multi-user product.

---

# 23. Deferred Backlog

Core items carried forward from the original roadmap that are not part of the
active Phase A–I sequence. They remain on the roadmap but are not scheduled.

## 23.1 Tag-based model permissions

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

## 23.2 Catalogue search and filtering

Enhance admin catalogue search by:

- provider;
- model family;
- capability;
- cost;
- availability;
- tags.

## 23.3 Rules-based routing

Potential rules:

- by client key;
- by request size;
- by protocol;
- by required capability;
- by time of day;
- by provider health.

This is intentionally far beyond the active phases.

## 23.4 Rate limits, quotas and budgets

Possible future controls:

- requests/minute per key;
- tokens/minute per key;
- daily/monthly budget;
- provider budget;
- model budget;
- concurrent request limits.

These require reliable usage accounting first (ties to Phase A and Phase H).

## 23.5 API tokens for automation

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

## 23.6 Multiple credentials per provider instance

Potentially allow one logical provider instance to contain a credential pool.

This should only be added if there is a real need for provider-side load
distribution or quota management. The pattern of multiple uniquely named
provider instances is simpler and should remain valid.

## 23.7 Protocol compatibility matrix

Expose a matrix such as:

```text
Provider/model       Chat   Responses   Messages   Tools   Streaming
...
```

Useful when choosing virtual targets.

## 23.8 Weighted routing

Possible future virtual model:

```text
main/general
  90% openai/gpt-5
  10% openrouter/anthropic/claude
```

Explicitly deferred. The starting bias is ordered targets only, no weighting.

## 23.9 Rename compatibility window

When renaming:

```text
main/coding -> main/dev
```

optionally retain the old name temporarily as an alias. A possible extension of
aliases (Phase F). V1 treats rename as breaking.

## 23.10 Hash-hardening / multiple valid key hashes

V1 stores hash-only client secrets using Argon2id and a non-secret selector.

Future improvement: multiple valid key hashes during planned rotations. A
possible extension of graceful client-key rotation (§14).

## 23.11 Configuration-only export

Add a portable JSON/YAML export that omits runtime/cache data.

Potential contents:

- provider definitions;
- virtual mappings;
- client metadata;
- permission structure.

Provider credentials should be optionally excluded. Client plaintext keys can
never be exported because they are not stored.

---

# 24. Roadmap Guardrail

Before implementing any roadmap feature, confirm:

1. V1 remains stable.
2. The feature solves an observed problem.
3. It does not require an unnecessary external service.
4. It does not weaken the simple `/opt/tiller-router` Docker deployment.
5. It does not make model-selection semantics harder to understand.
6. It does not silently change deterministic routing behavior.
7. It does not introduce prompt/response storage by default.

If the feature fails those tests, it should probably live somewhere other than
Tiller Router.

---

# 25. Explicit Anti-Roadmap

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
- Kubernetes support of any kind (manifests, Helm chart, or operation model).
  This project targets Docker Compose only.
- Complex billing system.

These are precisely the features that turn a small router into a large
platform.
