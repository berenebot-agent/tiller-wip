# TODO — Single-Model Client Keys

**Project:** Tiller Router  
**Status:** Implemented
**Scope:** Add a second client-key mode that exposes one stable client-facing model and centrally binds that key to one real or virtual model.

---

# 1. Objective

Tiller Router currently assumes a client key may expose a catalogue of permitted models.

Some clients, especially custom-provider integrations that do not reliably discover `/v1/models`, are easier to manage if the client is configured once with a single stable model name and Tiller controls the actual route centrally.

Add two client-key types:

```text
Catalogue
Single
```

The goal of `Single` mode is:

> Configure the client once, then change its actual model from the Tiller Client Keys page without touching the client again.

---

# 2. Client Key Types

## 2.1 Catalogue

Current behaviour.

A Catalogue key:

- exposes its permitted real and virtual models;
- uses the existing model-permission UI;
- validates the model requested by the client;
- routes that requested model if permitted;
- returns the permitted catalogue from `/v1/models`.

Existing client keys should remain Catalogue keys by default so current behaviour does not change.

---

## 2.2 Single

A Single key exposes one client-facing model name and binds the key to one Tiller route.

Conceptually:

```text
Client key: OpenCode
Type:       Single
Model name: main
Target:     main/coding
```

The target may be:

```text
a real model
OR
a virtual model
```

Example direct binding:

```text
OpenCode key
  client model: main
        ↓
  deepseek/deepseek-v4-pro
```

Example virtual binding:

```text
OpenCode key
  client model: main
        ↓
  main/coding
        ↓
  virtual-model routing logic
```

A Single key does not require the selected target to also be enabled through the Catalogue permission matrix.

The binding itself authorises that route.

---

# 3. Client-Facing Model Name

Default:

```text
main
```

The model name is editable per Single client key.

Examples:

```text
main
default
coding
gpt-4
```

This exists for client compatibility and convenience.

It is not a global Tiller model name.

Two different Single keys may both expose:

```text
main
```

while routing to completely different targets.

Changing the client-facing model name after deployment is allowed, but the UI should warn that this may require a client configuration change.

---

# 4. Single-Key Routing Semantics

For a Single key, the requested model name does NOT control routing.

The key's configured binding is authoritative.

Example:

```text
Single key:
client-facing model = main
bound target = main/coding
```

All of these requests:

```text
model = main
model = gpt-5
model = claude
model = default
model = typo123
```

route to:

```text
main/coding
```

provided the request is otherwise valid.

This is deliberate.

It improves compatibility with clients that:

- hard-code model IDs;
- require a manually entered model;
- do not discover custom-provider models correctly;
- send an unexpected model ID;
- contain a user typo in local model configuration.

The security boundary remains the client key:

> A Single key authorises exactly one configured route, regardless of what model ID the client sends.

The requested model must not allow the client to escape the configured binding.

---

# 5. Catalogue-Key Routing Semantics

Catalogue keys retain strict current behaviour:

```text
requested model
    ↓
check key permission
    ↓
route permitted requested model
```

So:

```text
Catalogue key
request = hidden/unpermitted model
→ reject
```

The "ignore requested model" behaviour applies only to Single keys.

---

# 6. `/v1/models`

For a Single key, `/v1/models` returns exactly one model:

```text
<configured client-facing model name>
```

Normally:

```text
main
```

It must not expose:

- the bound real model;
- the bound virtual model;
- other models available in Tiller;
- the Catalogue permissions attached to the key.

Example:

```json
{
  "data": [
    {
      "id": "main",
      "object": "model"
    }
  ]
}
```

Exact response shape should follow the current `/v1/models` implementation.

---

# 7. Response Model Identity

Where Tiller currently rewrites model identity safely, responses for a Single key should report the configured client-facing model name.

Example:

```text
Client sends:
model = gpt-5

Single key exposes:
main

Bound target:
main/coding

Response model:
main
```

Do not leak the real or virtual target to the client unless a protocol requires otherwise.

---

# 8. Target Types

The Single-key target selector must support:

```text
Real models
Virtual models
```

A real target routes directly through the existing provider/model routing path.

A virtual target routes through the existing virtual-model resolver.

If Virtual Models v2 later adds Ordered fallback:

```text
Single key
main
    ↓
main/coding
    ↓
Claude
    ↓ eligible failure
GPT
```

The Single key itself does not implement fallback.

It merely binds the client to the virtual model.

---

# 9. Key Design Separation

Keep these responsibilities distinct:

```text
Client Key binding
= What model/route does this client get?

Virtual Model
= How does this logical model route?
```

A Single client key may point:

```text
directly to a real model
```

or:

```text
to a virtual model with its own routing mode
```

Do not automatically create hidden virtual models for Single keys.

---

# 10. Client Keys UI

The main UX requirement is that Single-key routing can be changed directly from the Client Keys list.

Suggested table/card layout:

```text
Client       Type        Client model    Route
--------------------------------------------------------------
OpenCode     Single      main            [ main/coding       ▼ ]
Hermes       Single      main            [ glm/glm-5.3-flash ▼ ]
Treasurer    Single      default         [ main/accounting   ▼ ]
API Test     Catalogue                   Manage models
```

For Single keys, the Route field should be a searchable dropdown/typeahead.

The administrator should NOT need to open the client-key detail/edit screen merely to switch its model.

This inline route selector is a primary feature, not optional polish.

---

# 11. Inline Route Selector

The selector should search/group:

```text
Virtual models
  main/coding
  main/fast
  main/accounting

Real models
  anthropic/...
  openrouter/...
  openai/...
  deepseek/...
  glm/...
```

Selecting a new target:

- saves immediately;
- applies to new requests immediately;
- does not rotate/change the client API key;
- does not require Tiller restart;
- does not require client reconfiguration.

In-flight requests continue with the target resolved when that request began.

---

# 12. Create/Edit Client Key UI

Client-key creation/configuration should include:

```text
Type
[ Catalogue ▼ ]
```

Options:

```text
Catalogue
Single
```

If Single is selected, show:

```text
Client-facing model name
[ main ]

Target
[ Search real or virtual models... ]
```

If Catalogue is selected, show the current model permission configuration.

Keep this simple.

Do not add extra strategy/policy controls to the key.

---

# 13. Existing Keys

Existing keys should behave as:

```text
type = catalogue
```

with no behavioural change.

If the database needs a new column, use a default that preserves current Catalogue behaviour.

This is still a development/test deployment, so do not over-engineer migration complexity beyond what the existing repository architecture requires.

---

# 14. Switching Key Type

## Catalogue → Single

Require:

```text
client-facing model name
target
```

before save.

Existing Catalogue permissions may remain stored but inactive.

This keeps switching reversible.

## Single → Catalogue

Restore normal Catalogue behaviour and existing stored permissions.

The previous Single binding may remain stored but inactive if that is simplest and cleanest.

Do not let inactive Single binding state affect Catalogue routing.

---

# 15. Target Availability

A Single key should continue exposing its configured client-facing model from `/v1/models` even if the current target later becomes unavailable.

Example:

```text
main
  ↓
provider/model unavailable
```

`/v1/models` still exposes:

```text
main
```

Inference should return:

```text
503 model unavailable
```

The Client Keys UI should clearly show the broken/unavailable binding.

Do not silently choose another model unless the selected target is a Virtual Model whose own routing mode permits fallback.

---

# 16. Disabled / Retired Target

If the bound real target becomes administratively disabled or disappears from provider discovery:

- keep the binding;
- show it as unavailable/broken;
- return 503 for inference;
- do not silently rebind the key.

If the bound target is a virtual model, let the virtual model's own routing semantics determine whether it remains usable.

---

# 17. Target Deletion

Deletion of a real or virtual model that is actively bound to a Single key is blocked.

The admin must repoint the affected key first. Provider deletion is likewise
blocked when one of its real models is bound to a Single key.

Do not silently reassign a Single key to another model.

---

# 18. Capability Metadata

The `/v1/models` entry for a Single key should inherit the effective metadata of its selected route where Tiller supports such metadata.

Examples:

```text
context_length
max_output_tokens
input/output modalities
tool support
structured output
reasoning
streaming
```

If bound directly to a real model:

```text
effective metadata = real model metadata
```

If bound to a virtual model:

```text
effective metadata = virtual model effective metadata
```

If Virtual Models v2 later aggregates multiple fallback targets conservatively, the Single key should simply inherit that result.

Do not duplicate capability logic inside the Single-key implementation.

---

# 19. Activity Logging

Activity should retain both the client request and Tiller routing abstraction.

Useful conceptual fields:

```text
client_requested_model
client_exposed_model
bound_target
final_provider
final_model
```

Example:

```text
client_requested_model = gpt-5
client_exposed_model   = main
bound_target           = main/coding
final_provider         = anthropic
final_model            = claude-...
```

This makes compatibility quirks/fat-fingered model names visible to the administrator without causing request failure.

No prompt/response content.

---

# 20. API-Key Rotation

Rotating a Single client key must preserve:

```text
key type
client-facing model name
bound target
stored Catalogue permissions
```

Only the secret changes.

---

# 21. Admin API

Extend the current client-key API rather than building a separate routing subsystem.

Conceptually add:

```text
type
single_model_name
single_target_type
single_target_id
```

Possible values:

```text
type:
  catalogue
  single

single_target_type:
  real
  virtual
```

Exact schema/API naming should match current repository conventions.

The implementation planner should consider whether a single polymorphic target reference or separate nullable real/virtual IDs best fits the existing codebase.

Do not add unnecessary abstraction.

---

# 22. Runtime Resolution Flow

Conceptually:

```text
authenticate client key
    ↓
inspect key type
```

Catalogue:

```text
requested model
    ↓
permission check
    ↓
resolve requested real/virtual model
    ↓
route
```

Single:

```text
ignore requested model for routing
    ↓
load key's configured binding
    ↓
resolve real/virtual target
    ↓
route
    ↓
rewrite response identity to client-facing model name
```

The actual client-requested model should still be available to logging/diagnostics.

---

# 23. Protocol Coverage

Single-key behaviour must work consistently across the existing Tiller protocol scope:

```text
OpenAI /v1/chat/completions
OpenAI /v1/responses
Anthropic /v1/messages
```

Include:

- streaming;
- non-streaming;
- tool/function calls;
- reasoning/thinking where already supported;
- cancellation;
- model-name rewriting;
- upstream error handling.

Do not add OpenCode-specific proxy logic.

This is a generic client-key routing feature.

---

# 24. Security Requirements

Single mode must never allow the client-supplied model value to influence access to another route.

Example:

```text
Single key bound to:
main/coding

Client sends:
model = hidden/provider/model
```

Tiller must still route only to:

```text
main/coding
```

This means the Single-key binding acts as an allowlist of exactly one route.

Do not:

- fall through into Catalogue permissions;
- resolve the client-requested model;
- allow hidden models to be accessed by name.

---

# 25. Acceptance Tests

At minimum:

## Catalogue compatibility

1. Existing keys default to Catalogue.
2. Existing model permission behaviour unchanged.
3. Hidden/unpermitted model requests still rejected.

## Single basic routing

4. Single key exposes exactly one `/v1/models` entry.
5. Default client-facing name `main`.
6. Custom client-facing model name works.
7. Single key can bind to a real model.
8. Single key can bind to a virtual model.

## Requested model ignored

9. Request `model=main` routes to binding.
10. Request `model=gpt-5` routes to same binding.
11. Request `model=typo123` routes to same binding.
12. Client-requested model cannot escape to another real/virtual model.

## Response identity

13. Response model is rewritten to configured client-facing model name where supported.

## Quick switching

14. Change Single target from Client Keys list.
15. New request immediately uses new target.
16. No API-key change.
17. No restart.
18. In-flight request continues using previously resolved target.

## Virtual target

19. Binding to virtual model uses existing virtual resolver.
20. Hidden underlying real model remains inaccessible directly.
21. Future virtual fallback behaviour remains owned by virtual model, not Single key.

## Availability

22. Broken direct target keeps `/v1/models` entry.
23. Broken direct target returns 503.
24. No silent reassignment.

## Type switching

25. Catalogue → Single requires name + target.
26. Single → Catalogue restores Catalogue behaviour.
27. Stored Catalogue permissions remain intact if retained by implementation.

## Rotation

28. Client-key rotation preserves Single configuration.

## Activity

29. Log actual client-requested model.
30. Log exposed model/bound target/final route.
31. No prompt/response body logging.

## Protocol regression

32. Chat Completions.
33. Responses.
34. Anthropic Messages.
35. Streaming.
36. Tool calls.
37. Cancellation.
38. Existing routing tests remain green.

---

# 26. Explicit Non-Goals

Do not implement as part of this feature:

```text
multiple exposed aliases per Single key
small curated multi-model key mode
fallback logic on the client key itself
weighted routing
round-robin
pricing routing
time-based routing
multi-user/workspaces
OpenCode-specific endpoint behaviour
automatic client configuration
```

A future curated-key mode may be considered if real client needs emerge, but Single mode should remain exactly one exposed logical model.

---

# 27. UX Definition of Done

The target experience is:

1. Create a client key.
2. Choose:
   ```text
   Type = Single
   ```
3. Leave:
   ```text
   Model = main
   ```
4. Select:
   ```text
   Target = main/coding
   ```
5. Configure the client once with the Tiller endpoint, key and any model name it accepts.
6. From then on, change the client's real model directly from the Tiller **Client Keys** page using the inline searchable route selector.

Example:

```text
OpenCode   Single   main   [ Claude ▼ ]
```

Change to:

```text
OpenCode   Single   main   [ GPT ▼ ]
```

The next OpenCode request uses GPT without changing OpenCode.

That steering experience is the primary purpose of this feature.

---

# 28. Definition of Done

Single-Model Client Keys are complete when:

1. Client keys support `Catalogue` and `Single` types.
2. Existing keys retain current Catalogue behaviour.
3. Single keys expose exactly one editable client-facing model name.
4. Single keys bind to exactly one real or virtual model.
5. Client-supplied model IDs are ignored for routing on Single keys.
6. The binding cannot be escaped by requesting another model.
7. `/v1/models` returns exactly the Single key's exposed model.
8. Response identity remains the exposed model where safely supported.
9. Target can be changed inline from the Client Keys main page.
10. Target changes affect new requests immediately.
11. Virtual targets reuse existing virtual routing logic.
12. Broken targets remain explicit and do not silently reroute.
13. Capability metadata is inherited from the selected route.
14. Activity preserves actual requested model and actual routed target.
15. All current protocol/routing/security regressions remain green.
16. The implementation remains lightweight and aligned with Tiller's existing SQLite/single-container architecture.
