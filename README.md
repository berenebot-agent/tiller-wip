# Tiller Router

Tiller Router is a small, self-hosted LLM model-selection proxy. Each client
keeps one endpoint and one key while an administrator controls its visible
catalogue and maps stable virtual model names to real upstream targets.

V1 is one statically compiled Go service with embedded SQLite and an embedded
admin UI. It has no external database, no named volumes, and no prompt or
response logging. It supports ordered, pre-stream fallback for virtual models
only; direct real-model requests never fall back.

## Install under `/opt/tiller-router`

1. Copy this repository to `/opt/tiller-router`.
2. Create the bind-mounted state directory and make it writable by the chosen
   runtime UID/GID (defaults to `65532:65532`):

   ```sh
   cd /opt/tiller-router
   mkdir -p data
   chown 65532:65532 data
   ```

3. Copy `.env.example` to `.env` and replace both administrator credentials.
4. Build and start the one service:

   ```sh
   docker compose up -d --build
   ```

The router is currently published on `0.0.0.0:${TILLER_TEST_PORT:-8080}` for
direct LAN access. Reverse-proxy networking is deferred; when it returns, the
router should join the external proxy network and stop publishing a host port.
Do not configure a proxy to buffer SSE responses.

The container runs with a **read-only root filesystem** and all Linux
capabilities dropped. Persistent state is written only beneath the `./data`
bind mount; `/tmp` is an ephemeral tmpfs (64 MiB, `noexec,nosuid,nodev`) used
for SQLite/runtime temporary files and is wiped on every restart.

## First configuration

1. Open the HTTPS administrator URL and sign in with the environment-supplied
   account.
2. Add a provider. The provider is retained even if initial discovery fails.
3. Create a virtual provider group and virtual model if clients should not see
   the real provider namespace.
4. Create a client key and copy the secret from the one-time dialog.
5. Open that key's Permissions view and enable individual real or virtual
   models.

All permissions start OFF. A group's **New models default** setting is only a
feeder for models discovered or created later; changing it never modifies an
existing model permission.

## Operations

The binary supports:

```text
tiller-router serve
tiller-router migrate
tiller-router healthcheck
```

Migrations also run automatically before the HTTP server starts. Liveness is
at `/health/live`; readiness is at `/health/ready` and depends on SQLite and
migrations, not upstream availability.

Persistent state is exclusively `./data/tiller-router.db`. SQLite uses foreign
keys, WAL mode, and a busy timeout. Provider catalogue refresh runs
approximately every 24 hours with deterministic jitter; manual refresh is
available in the UI.

When a provider's `/models` endpoint does not report capability metadata
(context length, max output, tool/vision/reasoning/structured-output flags,
modalities), the router fills the gaps from the community-maintained
[models.dev](https://models.dev) registry. Provider-reported values are always
authoritative; models.dev only supplies what the provider left unknown, and a
field stays unknown if neither source reports it. The dataset is cached at
`./data/models-dev.json` (refreshed daily and on manual catalogue refresh) so
discovery never depends on models.dev being reachable. Set
`TILLER_MODELS_DEV_ENABLED=false` to disable the lookup entirely for offline or
privacy-sensitive deployments.

### Notifications

Settings includes an installation-global outbound webhook for routing events.
Enable it, set any HTTP(S) endpoint (an
[ntfy](https://ntfy.sh) topic is the simplest self-hosted example), and pick the
events: *Fallback occurred* (an ordered-fallback virtual model advanced to a
later target) and *All targets failed* (every eligible target was attempted and
none succeeded). Payloads are metadata-only JSON with the same privacy boundary
as Activity — no prompts, responses, or credentials — plus a short human-readable
summary. Delivery is best-effort with a short timeout: a failed notification is
recorded in diagnostics and never fails, delays, or alters an inference request,
and there is no queue or retry engine. An optional `Authorization` header is
stored for endpoints that need one; it is write-only (never displayed again) and
can be cleared from the settings card. A **Send test notification** button
verifies delivery before enabling events.

### Backup and restoration

The Backup/System screen downloads a consistent SQLite snapshot. The response
is administrator-authenticated, marked `no-store`, and explicitly identifies
secret material.

> A backup contains recoverable provider API credentials. Protect it like the
> credentials themselves. Client keys remain Argon2id hashes and cannot be
> recovered from the backup.

To restore:

1. Stop the Compose service.
2. Preserve the current `data/` directory separately.
3. Place the exported file at `data/tiller-router.db`, owned by the configured
   UID/GID, with mode `0600`.
4. Start the service. Existing client secrets remain valid because their hashes
   were restored.

Never copy only a live SQLite main file while WAL writes are active; use the
authenticated export.

## Security boundaries

- Client keys have a non-secret selector and a 32-byte secret. Only an Argon2id
  PHC hash of the secret is stored (64 MiB, 3 iterations, 4 lanes).
- Provider credentials are write-only through the UI/API but remain recoverable
  in SQLite until post-V1 encryption-at-rest work ships.
- Admin sessions are persistent (survive container restarts), default to a 30-day
  sliding-expiry lifetime, use HTTP-only SameSite cookies, and require CSRF tokens
  for mutations. The raw session secret is never stored; only an Argon2id hash is
  persisted. A material admin credential change invalidates all existing sessions.
- Prompt bodies, response bodies, tool arguments, credentials, and
  authorization headers are never logged.
- Upstream redirects are disabled, client authorization/cookie/organization/
  project headers are not forwarded, and only stored provider credentials are
  applied.
- Routing is deterministic. There is no retry, no health-based reroute, and no
  alternate-model selection. The only fallback is ordered and pre-stream, and
  applies to virtual models configured for ordered fallback only; it is
  non-silent (recorded and visible in Activity) and direct real-model requests
  never fall back.
- The container root filesystem is read-only with all Linux capabilities
  dropped and `no-new-privileges` enforced; `/tmp` is an ephemeral tmpfs, so
  nothing written there survives a restart.

## Client configuration

See [docs/client-configuration.md](docs/client-configuration.md) for Hermes
Agent's three API modes, OpenCode, Codex CLI, Claude Code, SDK, and cURL
examples.

## Development and verification

All dependency resolution, building, and Go tests run inside Docker via the
`./tiller-go.sh` wrapper, which uses persistent bind-mounted caches and a RAM
cap (no Go needs to be installed on the host). The browser and compatibility
tests are fully containerized and need no host Go either. The test Dockerfiles
use BuildKit RUN cache mounts and every test build passes `--pull=false`, so
base images and package downloads (the large Playwright base image, apk/npm/pip
packages) are reused locally instead of being re-downloaded on every run:

```sh
./tiller-go.sh mod tidy
./tiller-go.sh test ./...
docker build --pull=false -t tiller-router:dev .
./tests/browser/run.sh        # build + start router & mock from a fresh temp DB, run Playwright, teardown
./tests/compatibility/run.sh  # SDK + Codex/OpenCode/Claude-Code/Hermes probes
```

`./tests/browser/run.sh` builds the router and browser images once, starts the
mock upstream and a router from a **fresh ephemeral data dir** (so no state
leaks between runs), runs the Playwright suite, and tears everything down —
the single documented entry point for the UI tests.

The test suite covers Argon2id key handling, migrations and namespace
constraints, consistent backup, provider registry/discovery pagination,
two-client catalogue isolation, non-retroactive permission feeders,
guessed-model rejection, hidden-target virtual routing, immediate remapping,
all three streaming protocols, tool-call chunks, disconnect cancellation,
rotation and disable invalidation, failed-refresh preservation, retired
targets, provider outages, ordered fallback and fallback exhaustion for virtual
models, and backup restoration with existing client keys.
The disposable compatibility probes use pinned official OpenAI and Anthropic
Python SDKs plus real Codex CLI, OpenCode, Claude Code, and Hermes Agent
executables against a controllable mock upstream. Hermes is exercised in Chat
Completions, Codex Responses, and Anthropic Messages modes. The browser image
runs the admin workflow with Playwright.

The frozen implementation contract is
[docs/tiller-router-v1-specification.md](docs/tiller-router-v1-specification.md).
