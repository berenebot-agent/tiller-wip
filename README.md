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

> **Beta software.** Tiller Router is in a public beta. The routing core and deployment model are approaching stability, but the API and supported-provider surface may still evolve before a stable `1.0`. Feedback is welcomed but use with caution.

---

## Why Tiller exists

1. **Every tool had its own (bad) model selector.** Some bury it in settings, some want model IDs in config files, some barely support changing models at all. Tiller moves model selection out of the tool and into one control panel — change it without touching the clients.
2. **Cheap and free API access is useful — until it rate-limits.** Tiller gives virtual models an **ordered fallback chain**, so a limited provider is useful without becoming a single point of failure.
3. **Provider API keys were scattered everywhere.** Enter provider credentials once in Tiller; clients only ever receive a Tiller key.

---

## What Tiller does

Tiller sits between your LLM clients and your upstream providers. Clients keep a stable **endpoint, API key and model name**. You change what sits behind it — a route change applies to new requests immediately. Regardless of what model the client requests, Tiller sends the request to your selected model and the client need know no difference. No restart, no CLI, no config editing. Steer a client key from the control panel by picking any real or virtual model:

<p align="center">
  <img src="assets/media/screenshots/instant-switch-models.png" width="840" alt="Instant model switch from the Tiller control panel">
</p>

> **Tiller does not try to choose the model for you. It gives you the tiller.**

---

## Client keys

Two client key types cover the two ways tools talk to an LLM API:

### Single — one key, one route

Exposes one stable model identity — usually something simple like `main` — bound to any real or virtual model. The key itself defines the route; the model string the client sends cannot escape that binding. Change the route from the control panel and the next request follows it.

### Catalogue — a controlled model list

Exposes a controlled subset of Tiller's catalogue through `GET /v1/models`, with per-client model permissions. Useful to curate exactly which providers and models appear for a client to save wading through hundreds of rows of models you don't use.

---

## Virtual models

A virtual model gives a stable client-facing identity to one or more upstream targets. For example, `main/coding` might resolve to:

```text
1. Z.ai / GLM
2. DeepSeek
3. OpenRouter / Claude
```

The client only knows `main/coding`. You can reorder or replace the targets without changing the client.

Configure the ordered target list in the control panel:

<p align="center">
  <img src="assets/media/screenshots/auto_fallback.png" width="420" alt="Editing a virtual model's ordered fallback list">
</p>

**Ordered fallback:** if an upstream attempt fails before client-visible output begins, Tiller may try the next configured target. Once output has started, it never splices another model into the response. There is no hidden health-based or random routing — the order you configure is the order Tiller uses.

Each attempt is visible in Activity, including the fallback — the client gets a valid response and never knows there was a failure:

<p align="center">
  <img src="assets/media/screenshots/fallback.png" width="840" alt="Activity view showing a failed attempt followed by a successful fallback">
</p>

---

## Features

- **Steering** — fast web control panel; single and catalogue client keys; real and virtual models in one route selector; immediate route changes; stable client-facing identities.
- **Providers & models** — multiple named instances; credentials entered once; automatic catalogue discovery; manual/periodic refresh; retired models preserved rather than silently remapped; context/capability metadata.
- **Routing** — fixed virtual routes; ordered fallback; configurable fallback timeout; no silent fallback on direct real-model calls; no response-stream splicing after output begins; client cancellation propagates upstream.
- **Client API** — `GET /v1/models`, `POST /v1/chat/completions`, `POST /v1/responses`, `POST /v1/messages`, covering the common OpenAI and Anthropic surfaces with safe protocol translation.
- **Activity** — searchable, filterable, CSV-exportable request metadata (client, model, route, provider, status, latency, tokens, fallbacks). Client request bodies and provider error responses are never retained unless you turn on detailed error logging. Retention controlled per client key.
- **Notifications** — optional best-effort webhooks for fallback, all-targets-failed, key created/deleted, admin login. Metadata-only, never blocks inference.
- **Operations** — persistent admin sessions; key rotation; SQLite backup export; health endpoints; single Docker container; embedded UI; read-only rootfs; non-root runtime user.

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

Provider support varies because upstream APIs vary. The beta should be treated as **verified for the providers explicitly tested** and compatibility/best-effort for the wider OpenAI-compatible surface.

---

## Install

Requires Docker and Docker Compose.

### Option 1 — Prebuilt image (recommended)

1. **Prepare a directory:**

   ```bash
   mkdir tiller-router && cd tiller-router
   ```

2. **Create a `docker-compose.yml`:**

   ```yaml
   services:
     tiller-router:
       container_name: tiller-router
       image: ghcr.io/dellarb/tiller-router:latest
       ports:
         - "8080:8080"
       environment:
         TILLER_ADMIN_USERNAME: admin
         TILLER_ADMIN_PASSWORD: replace-this-with-a-long-random-password
       volumes:
         - ./data:/data
       restart: unless-stopped
   ```

   That's the whole setup — there is no data-directory step. Docker creates `./data` automatically on first `up`, and the container fixes its ownership itself at boot (starts as root, hands `./data` to the runtime user, drops privileges before serving — default `65532:65532`, or your own uid:gid via `TILLER_RUN_UID`/`TILLER_RUN_GID`, see below).

3. **Start Tiller:**

   ```bash
   docker compose up -d
   ```

   Then open `http://localhost:8080` and log in. For remote access, put Tiller behind an HTTPS reverse proxy.

> **Reverse proxy + live UI:** the admin UI keeps its status icons and usage counters live over a Server-Sent Events stream at `/api/admin/live`. If you front Tiller with a reverse proxy, disable response buffering for that path (e.g. nginx `proxy_buffering off;` or Caddy's equivalent) and keep the proxy's read timeout above the stream's 5s heartbeat, or the stream will stall. The stream is in-process and single-instance by design — it does not span multiple Tiller containers.

### Option 2 — Build from source

```bash
git clone https://github.com/dellarb/tiller-router.git
cd tiller-router
cp .env.example .env   # set TILLER_ADMIN_USERNAME / TILLER_ADMIN_PASSWORD
docker compose up -d --build
```

The repository's `docker-compose.yml` builds locally instead of pulling an image; everything else (healthcheck, adoption-first posture, volumes) is identical to the prebuilt option above.

The service starts as root to self-fix `./data` ownership, then drops to a non-root user before serving.

### Other compose / env options

The repo's `docker-compose.yml` plus `.env` cover the most common customisations without editing any Go code. The full list of recognised variables is in `.env.example`.

```bash
TILLER_ADMIN_USERNAME=admin                          # admin login for the web UI
TILLER_ADMIN_PASSWORD=replace-with-a-long-random-password   # admin password
TILLER_PORT=8080                                     # host port (default 8080)
TILLER_RUN_UID=1000                                  # runtime uid (default 65532)
TILLER_RUN_GID=1000                                  # runtime gid (default 65532)
TILLER_UID=1000                                      # build-time uid for baked-in files (default 65532)
TILLER_GID=1000                                      # build-time gid for baked-in files (default 65532)
TILLER_TRUSTED_PROXY=10.1.1.12                       # IP/CIDR of reverse proxy if using one
TILLER_MODELS_DEV_ENABLED=true                       # models.dev metadata (default true)
TILLER_ADMIN_SESSION_TTL=720h                        # admin session lifetime (default 720h)
```

### First steps

1. **Add a provider** — in **Providers**, `+ Add provider`. Choose the type, name the instance, add its API credential. Tiller discovers the model catalogue where supported.

2. **Create a route** — use a real model directly, or create a virtual model such as `main/coding` with one or more ordered targets.

3. **Create a client key** — for the simplest setup: `Type: Single`, `Client model: main`, `Route: main/coding`. Tiller shows the client secret once.

4. **Point your tool at Tiller** — for an OpenAI-compatible client: `Base URL: http://localhost:8080/v1`, `API key: <your Tiller client key>`, `Model: main`.

Now steer the real route from Tiller instead of changing the client.

---

## API example

```bash
# List the models visible to a client key
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer $TILLER_API_KEY"

# Send a Chat Completions request
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

## Architecture

Tiller is intentionally small: a Go HTTP server (client API, provider adapters, protocol translation, route/fallback resolver, admin API, embedded control-panel assets) over SQLite (providers, model catalogue, virtual routes, client keys, sessions, settings, Activity).

No Redis. No Postgres. No separate frontend service. No message broker. No vector database. The normal deployment is one container with one bind-mounted data directory.

---

## Design principles

- **Steerable over clever** — if you configure `A → B → C`, Tiller won't decide it prefers `C → A → B` based on an opaque score.
- **Stable clients, movable backends** — the client should know as little as possible about the real provider arrangement.
- **Failure should be boring** — a rate-limited upstream falls through to the next target without turning into an emergency reconfiguration.
- **Keep infrastructure small** — Go, SQLite, one container, few dependencies.
- **Don't collect content you don't need** — Activity answers "what route did this request take?", not "what did it say?". Activity is metadata-only by default. If the administrator explicitly enables Detailed Error Logging, failed request bodies and provider error bodies may be stored, bounded to 1 MiB, and Activity exports containing those records must be treated as sensitive.

---

## What Tiller is not

Tiller is deliberately **not** an agent framework, LLM marketplace, billing platform, prompt-management suite, vector database, model benchmarking service, automatic "AI chooses the best AI" engine, multi-tenant SaaS control plane, or an attempt to reproduce every feature of a general-purpose LLM gateway.

It is a focused router for people who want to **control their own clients, providers and routes from one place**.

---

## Data and security

Tiller stores its state under the configured data directory, normally `./data`. This includes sensitive provider credential material.

**Provider credentials are not encrypted at rest.** They are stored in recoverable form in the SQLite database so Tiller can authenticate requests to your upstream providers; encryption at rest is a future-roadmap consideration. Take care with **where you store the persistent database** (`./data`) and any backups of it — keep them on storage you trust and treat them as secrets, since anyone who can read the database file can recover your provider keys.

Client API-key secrets are shown once and stored in hashed form for authentication. Provider credentials necessarily remain recoverable by Tiller so it can authenticate upstream requests. Activity is metadata-only by default. If the administrator explicitly enables Detailed Error Logging, failed request bodies and provider error bodies may be stored, bounded to 1 MiB, and Activity exports containing those records must be treated as sensitive. Migration 024 clears body columns from the live database, but this is not secure erasure: SQLite pages, WAL files, snapshots, and older backups may still contain historic sensitive data and must remain protected.

For anything other than local-only use: use HTTPS, put Tiller behind a trusted reverse proxy, use a strong admin password, protect the data directory and exported backups, and do not expose the control panel casually to the public internet. See [SECURITY.md](SECURITY.md).

**Container posture — hardening is opt-in.** Out of the box the container already runs with a read-only rootfs, a scratch `/tmp` tmpfs, and `no-new-privileges`: it starts as root at boot, self-fixes `./data` ownership, drops to the runtime user, and serves. For internet-exposed deployments you can go further by adding `cap_drop: [ALL]` and forcing a strict non-root `user:` — the repository's `docker-compose.yml` shows the exact commented block. Two rules: `cap_drop ALL` removes the capabilities the boot-time ownership fix needs, so it requires `user:` alongside it; and with `user:` set, the ownership fix is skipped, so `./data` must already exist owned by that user (otherwise startup fails with a logged remediation telling you the expected owner).

---

## Development

Tiller is written in Go with an embedded browser UI and SQLite.

```bash
go test ./...
go vet ./...
go build ./cmd/tiller-router
docker compose build
```

The project currently targets the Go version declared in `go.mod`.

---

## Project status

Tiller Router is in **beta**. The routing model is intentionally narrow and already useful, but the public API, migrations and provider compatibility surface may still evolve before a stable `1.0`. The near-term priority is reliability and hardening.

Likely post-beta areas include: additional provider validation, richer provider health, cost-aware routing, additional capability metadata, experimental subscription-backed providers such as Codex, and further hardening and operational polish.

---

## Contributing

Issues and pull requests are welcome. The project intentionally favours a small, understandable core — new features are most useful when they strengthen Tiller's central job:

> **Make LLM clients easy to steer without making the router itself complicated.**

See [CONTRIBUTING.md](CONTRIBUTING.md).

---

## Licence

See [LICENSE](LICENSE).
