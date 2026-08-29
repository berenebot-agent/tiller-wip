# Tiller Router

Tiller Router is a small, self-hosted LLM model-selection proxy. Each client
keeps one endpoint and one key while an administrator controls its visible
catalogue and maps stable virtual model names to real upstream targets.

V1 is one statically compiled Go service with embedded SQLite and an embedded
admin UI. It has no external database, no named volumes, no fallback routing,
and no prompt or response logging.

## Install under `/opt/tiller-router`

1. Copy this repository to `/opt/tiller-router`.
2. Create the bind-mounted state directory and make it writable by the chosen
   runtime UID/GID (defaults to `65532:65532`):

   ```sh
   cd /opt/tiller-router
   mkdir -p data
   chown 65532:65532 data
   ```

3. Copy `.env.example` to `.env`, replace both administrator credentials, and
   set the reverse-proxy network name.
4. Build and start the one service:

   ```sh
   docker compose -f docker-compose.yml -f docker-compose.proxy.yml up -d --build
   ```

The base Compose file publishes no host port. The proxy override joins the
existing external network named by `TILLER_PROXY_NETWORK`. Configure the
reverse proxy to send HTTPS traffic to `tiller-router:8080`. Do not configure
the proxy to buffer SSE responses.

For loopback-only testing:

```sh
docker compose -f docker-compose.yml -f docker-compose.test.yml up -d --build
```

This publishes only `127.0.0.1:${TILLER_TEST_PORT:-8080}`.

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
- Admin sessions last 12 hours, are process-local, use HTTP-only SameSite
  cookies, and require CSRF tokens for mutations.
- Prompt bodies, response bodies, tool arguments, credentials, and
  authorization headers are never logged.
- Upstream redirects are disabled, client authorization/cookie/organization/
  project headers are not forwarded, and only stored provider credentials are
  applied.
- Routing is deterministic. There is no retry, fallback, health-based reroute,
  or alternate-model selection.

## Client configuration

See [docs/client-configuration.md](docs/client-configuration.md) for Hermes
Agent's three API modes, OpenCode, Codex CLI, Claude Code, SDK, and cURL
examples.

## Development and verification

All dependency resolution, building, and tests run inside Docker:

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.26.7-alpine go mod tidy
docker run --rm -v "$PWD:/src" -w /src golang:1.26.7-alpine go test ./...
docker build -t tiller-router:dev .
docker build -t tiller-router-browser-tests:dev tests/browser
docker build -t tiller-router-sdk-probes:dev tests/compatibility
docker build -f tests/compatibility/hermes.Dockerfile -t tiller-router-hermes-probe:dev tests/compatibility
./tests/compatibility/run.sh
```

The test suite covers Argon2id key handling, migrations and namespace
constraints, consistent backup, provider registry/discovery pagination,
two-client catalogue isolation, non-retroactive permission feeders,
guessed-model rejection, hidden-target virtual routing, immediate remapping,
all three streaming protocols, tool-call chunks, disconnect cancellation,
rotation and disable invalidation, failed-refresh preservation, retired
targets, provider outages, and backup restoration with existing client keys.
The disposable compatibility probes use pinned official OpenAI and Anthropic
Python SDKs plus real Codex CLI, OpenCode, Claude Code, and Hermes Agent
executables against a controllable mock upstream. Hermes is exercised in Chat
Completions, Codex Responses, and Anthropic Messages modes. The browser image
runs the admin workflow with Playwright.

The frozen implementation contract is
[docs/tiller-router-v1-specification.md](docs/tiller-router-v1-specification.md).
