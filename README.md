<p align="center">
  <img src="internal/web/assets/media/tiller-mark.svg" width="88" alt="Tiller Router">
</p>

<h1 align="center">Tiller Router</h1>

<p align="center">
  <strong>One endpoint. One key per client. Steer the models behind it.</strong>
</p>

<p align="center">
  A lightweight, self-hosted LLM router with a control panel built for people who actually change models.
</p>

> **Alpha software.** Tiller Router is approaching its first public release. Back up your data and expect some rough edges while the project settles.

---

## Why Tiller exists

I had a growing collection of LLM agents, coding tools and automations, and the same three annoyances kept coming up.

### 1. Every tool had its own bad model selector

Some clients have decent model management. Some have a dropdown buried in settings. Some want model IDs in config files. Some barely support changing models at all.

I wanted to be able to say:

```text
OpenCode → use Claude today
Hermes   → use GLM
Agent X  → use DeepSeek
```

and change it again five minutes later **without touching the clients**.

Tiller moves model selection out of the agent/tool and into one fast control panel.

### 2. Cheap and free API access is useful — until it rate-limits

Free tiers, promotional credits and limited API keys are great upstreams, but they are not always reliable enough to make the client depend on them directly.

I wanted:

```text
try this model first
        ↓
if it fails, try this one
        ↓
then this one
```

with the client still asking for the same model name.

Tiller gives virtual models an **ordered fallback chain**, so a limited provider can be useful without becoming a single point of failure.

### 3. I was tired of putting provider API keys into everything

If ten tools all talk directly to five providers, credentials end up scattered through config files, containers and machines.

With Tiller:

```text
Provider credentials → Tiller
Client credentials   → each tool
```

Provider keys are entered once. Clients only receive a Tiller key.

---

## What Tiller does

Tiller sits between your LLM clients and your upstream providers.

```text
                         ┌──────────────────────┐
 OpenCode ──────────────▶│                      │────▶ OpenAI
 Hermes ────────────────▶│                      │────▶ Anthropic
 Coding agent ──────────▶│    Tiller Router     │────▶ OpenRouter
 Automation ────────────▶│                      │────▶ DeepSeek
 Random AI thing ───────▶│                      │────▶ GLM / Z.ai
                         │                      │────▶ Ollama
                         └──────────────────────┘────▶ ...
                                  ▲
                                  │
                           steer from the UI
```

The client keeps a stable:

```text
endpoint
API key
model name
```

You change what sits behind it.

A route change applies to new requests immediately. No client restart, config edit or credential swap required.

> **Tiller does not try to choose the model for you. It gives you the tiller.**

---

## The main idea: Single client keys

A **Single** client key exposes one stable model identity — usually something simple like:

```text
main
```

You bind that key to any real or virtual model in Tiller:

```text
OpenCode
model: main
    ↓
Tiller
    ↓
main/coding
    ↓
Anthropic / Claude
```

Then change it from the control panel:

```text
OpenCode
model: main
    ↓
Tiller
    ↓
main/coding
    ↓
DeepSeek
```

OpenCode still thinks it is using `main`.

For Single keys, the key itself defines the route. The model string supplied by the client does not let it escape that binding. This is useful for tools with awkward model selectors, hard-coded model names, or configs you simply do not want to keep editing.

The Client Keys screen is designed to be the steering surface:

```text
Client        Type       Client model     Route
OpenCode      Single     main             main/coding
Hermes        Single     main             glm/glm-...
Treasurer     Single     default          main/accounting
```

Change the route, and the next request follows it.

---

## Catalogue client keys

Not every client needs to be forced onto one route.

A **Catalogue** key exposes a controlled subset of Tiller's catalogue through:

```text
GET /v1/models
```

You decide which real and virtual models that client is allowed to see and use.

This is useful when the client has a good model picker but you still want:

- centralised provider credentials;
- per-client model permissions;
- stable virtual model names;
- one place to manage the catalogue.

---

## Virtual models

Virtual models give a stable client-facing identity to one or more upstream targets.

For example:

```text
main/coding
```

can resolve to:

```text
1. Z.ai / GLM
2. DeepSeek
3. OpenRouter / Claude
```

The client only knows:

```text
main/coding
```

You can reorder or replace the targets without changing the client.

### Ordered fallback

A virtual model can try targets in order.

```text
request
  │
  ▼
GLM
  │ upstream failure before output?
  ▼
DeepSeek
  │ upstream failure before output?
  ▼
OpenRouter
```

Tiller's rule is intentionally simple:

> **If an upstream attempt fails before client-visible output begins, Tiller may try the next configured target. Once output has started, Tiller will not splice another model into the response.**

That means provider errors, rate limits, unavailable models and other upstream failures can fall through to the next target while preserving a coherent response for the client.

There is no hidden health-based or random routing. The order you configure is the order Tiller uses.

---

## Features

### Steering

- Fast web control panel
- Single client keys with a centrally controlled route
- Catalogue client keys with per-model permissions
- Real and virtual models in the same route selector
- Route changes apply immediately to new requests
- Stable client-facing model identities

### Providers and models

- Multiple named provider instances
- Provider credentials entered once in Tiller
- Automatic model catalogue discovery
- Manual and periodic catalogue refresh
- Retired/unavailable model state is preserved rather than silently remapped
- Context length, output limits and capability metadata where available

### Routing

- Fixed virtual routes
- Ordered fallback across multiple targets
- Configurable fallback timeout
- No silent fallback on direct real-model calls
- No response-stream splicing after output has begun
- Client cancellation propagates upstream

### Client API surfaces

Tiller exposes:

```text
GET  /v1/models
POST /v1/chat/completions
POST /v1/responses
POST /v1/messages
```

These cover the common OpenAI and Anthropic client surfaces.

Tiller translates between supported protocol shapes where it can do so safely. Provider-specific stateful features are not always portable between backends, and Tiller prefers rejecting an unsupported translation over pretending it is equivalent.

### Activity

Tiller can keep request **metadata** so you can see what actually happened:

```text
client
requested model
resolved route
provider
upstream model
status
latency
token usage
fallback attempts
request ID
```

Activity can be searched, filtered and exported to CSV.

Tiller does **not** persist prompt or response content in Activity.

Logging and retention can be controlled per client key.

### Notifications

Optional best-effort webhook notifications can report events such as:

- fallback occurred;
- all targets failed;
- client key created/deleted;
- admin login.

Notification delivery is metadata-only and never blocks inference.

### Operations

- Persistent admin sessions
- Client key rotation
- Consistent SQLite backup export
- Health endpoints
- Single Docker container
- Embedded web UI
- SQLite persistence
- Read-only container root filesystem
- Non-root runtime user

---

## Supported providers

Tiller includes adapters for a broad set of native and OpenAI-compatible providers.

<details>
<summary><strong>Current provider types</strong></summary>

- OpenAI
- Anthropic
- OpenRouter
- DeepSeek
- Z.ai / GLM
- Google Gemini API
- Azure OpenAI
- Amazon Bedrock API key
- Groq
- Mistral
- xAI
- Together
- Fireworks
- Cerebras
- Perplexity
- NVIDIA NIM
- Hugging Face Inference
- Cloudflare Workers AI
- Alibaba / Qwen
- MiniMax
- OpenCode Zen
- OpenCode Go
- Ollama Local
- Ollama Cloud
- Generic OpenAI-compatible
- vLLM
- LM Studio
- llama.cpp

</details>

Provider support varies because upstream APIs vary. The first alpha should be treated as **verified for the providers explicitly tested and compatibility/best-effort for the wider OpenAI-compatible surface**.

---

## Quick start

### 1. Create your environment file

Create `.env`:

```env
TILLER_ADMIN_USERNAME=admin
TILLER_ADMIN_PASSWORD=replace-this-with-a-long-random-password
```

### 2. Start Tiller

```bash
docker compose up -d --build
```

Then open:

```text
http://localhost:8080
```

For remote access, put Tiller behind an HTTPS reverse proxy and configure trusted proxy handling appropriately.

### 3. Add a provider

In **Providers**:

```text
+ Add provider
```

Choose the provider type, give the instance a useful name, and add its API credential.

Tiller will discover the provider's model catalogue where supported.

### 4. Create a route

You can either use a real model directly or create a virtual model such as:

```text
main/coding
```

with one or more ordered targets.

### 5. Create a client key

For the simplest setup, create:

```text
Type: Single
Client model: main
Route: main/coding
```

Tiller shows the client secret once. Save it in your client.

### 6. Point your tool at Tiller

For an OpenAI-compatible client:

```text
Base URL: http://localhost:8080/v1
API key:  <your Tiller client key>
Model:    main
```

Now steer the real route from Tiller instead of changing the client.

---

## API example

List the models visible to a client key:

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer $TILLER_API_KEY"
```

Send a Chat Completions request:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $TILLER_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "main",
    "messages": [
      {
        "role": "user",
        "content": "Say hello from whichever model Tiller is currently pointing at."
      }
    ]
  }'
```

With a Single key, `main` can be redirected from the control panel without changing this request.

---

## A practical example

Suppose you have a limited API key that is excellent while quota is available.

Instead of configuring your coding tool directly:

```text
OpenCode
    ↓
Free provider
```

configure:

```text
OpenCode
    ↓
Tiller: main
    ↓
main/coding
    ├─ 1. Free / limited provider
    ├─ 2. Low-cost paid provider
    └─ 3. Reliable fallback provider
```

When the first provider hits a rate limit before it starts responding, Tiller tries the next target.

Tomorrow, if you want a completely different model to be first:

```text
drag / change route
```

The client configuration stays exactly the same.

---

## Architecture

Tiller is intentionally small.

```text
┌─────────────────────────────────────┐
│            Tiller Router            │
│                                     │
│  Go HTTP server                     │
│  ├─ client API                      │
│  ├─ provider adapters               │
│  ├─ protocol translation            │
│  ├─ route/fallback resolver         │
│  ├─ admin API                       │
│  └─ embedded control-panel assets   │
│                                     │
│  SQLite                             │
│  ├─ providers / model catalogue     │
│  ├─ virtual routes                  │
│  ├─ client keys / permissions       │
│  ├─ sessions                        │
│  ├─ settings                        │
│  └─ metadata-only Activity          │
└─────────────────────────────────────┘
```

No Redis.

No Postgres.

No separate frontend service.

No message broker.

No vector database.

The normal deployment is one container with one bind-mounted data directory.

---

## Design principles

### Steerable over clever

Tiller is designed around explicit operator control.

If you configure:

```text
A → B → C
```

Tiller should not decide that today it prefers:

```text
C → A → B
```

because of an opaque score.

More sophisticated routing may come later, but the configured route should always be understandable.

### Stable clients, movable backends

The client should know as little as possible about the real provider arrangement.

That is the point.

### Failure should be boring

A rate-limited upstream should be able to fall through to the next target without turning into an emergency reconfiguration exercise.

### Keep infrastructure small

Tiller is a router and control panel, not an infrastructure platform.

The bias is toward:

```text
Go
SQLite
one container
few dependencies
```

### Don't collect content you don't need

Activity is for answering:

> What route did this request take, and what happened?

It is not intended to become a prompt surveillance database.

---

## What Tiller is not

Tiller is deliberately **not**:

- an agent framework;
- an LLM marketplace;
- a billing platform;
- a prompt-management suite;
- a vector database;
- a model benchmarking service;
- an automatic "AI chooses the best AI" engine;
- a multi-tenant SaaS control plane;
- an attempt to reproduce every feature of a general-purpose LLM gateway.

It is a focused router for people who want to **control their own clients, providers and routes from one place**.

---

## Data and security

Tiller stores its state under the configured data directory, normally:

```text
./data
```

This includes sensitive provider credential material.

Treat the data directory and its backups as secrets.

Client API-key secrets are intended to be shown once and stored in hashed form for authentication. Provider credentials necessarily remain recoverable by Tiller so it can authenticate upstream requests.

Activity and notification records are metadata-only and should not contain prompt or response bodies.

For anything other than local-only use:

- use HTTPS;
- put Tiller behind a trusted reverse proxy;
- use a strong admin password;
- protect the data directory;
- protect exported backups;
- do not expose the control panel casually to the public internet.

See [SECURITY.md](SECURITY.md) for the project's security policy and reporting process.

---

## Backups

The control panel can export a consistent SQLite backup.

Backups contain provider credentials and must be protected accordingly.

The normal persistent data directory should also be included in your own host backup strategy.

---

## Development

Tiller is written in Go with an embedded browser UI and SQLite.

Run the test suite:

```bash
go test ./...
```

Static checks:

```bash
go vet ./...
```

Build:

```bash
go build ./cmd/tiller-router
```

Or build the container:

```bash
docker compose build
```

The project currently targets the Go version declared in `go.mod`.

---

## Project status

Tiller Router is in **alpha**.

The routing model is intentionally narrow and already useful, but the public API, migrations and provider compatibility surface may still evolve before a stable `1.0`.

The near-term priority is reliability and hardening rather than adding every possible routing feature.

Likely post-alpha areas include:

- additional provider validation;
- richer provider health;
- cost-aware routing;
- additional capability metadata;
- experimental subscription-backed providers such as Codex;
- further hardening and operational polish.

---

## Contributing

Issues and pull requests are welcome.

The project intentionally favours a small, understandable core. New features are most useful when they strengthen Tiller's central job:

> **Make LLM clients easy to steer without making the router itself complicated.**

See [CONTRIBUTING.md](CONTRIBUTING.md).

---

## Licence

See [LICENSE](LICENSE).
