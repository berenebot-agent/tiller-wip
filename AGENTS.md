# AGENTS.md — Tiller Router

Guardrails for any coding agent working in this repository. This file describes how the repo works and the coding rules to follow — it is not a specification document and does not restate the design.

## Source of truth

- Decisions live in code, in commit history, and in conversation with the human driving the change. If a request conflicts with existing code, surface the conflict and ask before proceeding — but do not treat the current implementation as an impassable wall in live dev.

## Scope discipline

- Before writing code for a new feature, confirm the scope with the human driving the change. Anything deferred is fine to build in live dev with explicit human sign-off, but never silently and never "just to see."
- Adding a brand-new dependency, service, or infrastructure component (Redis, Postgres, message queues, vector DBs, Kubernetes, etc.) still requires an explicit, named request from a human.
- Do not "clean up" the deferred-work backlog's phase ordering or scope on your own initiative. Backlog sequencing is a human decision.
- If a task requires touching something explicitly marked deferred (e.g. credential encryption or provider-health infrastructure) to complete the immediate ask, surface it and get sign-off rather than quietly building the deferred piece too.

## Deployment model

- Everything runs as a single Docker Compose service under `/opt/tiller-router/`.
- Bind mounts only. **Never** introduce a Docker named volume.
- All persistent state lives under `./data`. If you add any new persistent file, it goes under `./data` and must survive a container restart and a directory move to a different host with no other changes.
- No Kubernetes artifacts of any kind (manifests, Helm, operators). This is Compose-only.
- Don't add anything that requires a host-published port when a reverse-proxy Docker network is in use.
- Don't require Docker socket access or privileged mode.

**Out-of-box posture (default, adoption-first):** the default compose runs via an **in-process root-then-drop** (image has no baked-in `USER`, so the container starts as root; `internal/privdrop`, wired into `cmd/tiller-router`, `chown -R`s the data bind mount to `TILLER_RUN_UID`/`TILLER_RUN_GID` and then sheds privileges before the database is opened — pure Go, no shell entrypoint, no `su-exec`, works on the `scratch` image). This is what makes `docker compose up` work with a fresh `./data` dir — no manual `chmod`/ownership step needed. So out of the box the container is root at boot for the ownership fix, then non-root for the app. The default compose also ships `read_only` + `/tmp` tmpfs + `no-new-privileges`, which are compatible with the drop (only `./data` and `/tmp` are written, no setuid binaries are exec'd).

**Hardenable, not required:** the stricter posture adds `cap_drop: ALL` plus a strict non-root `user:` (commented block in `docker-compose.yml`). Those two are opt-in because they conflict with the drop: `cap_drop ALL` removes the chown/setuid capabilities the boot fix needs, and a strict `user:` skips the boot chown entirely (fresh root-owned `./data` then fails loud with `ErrDataDirUnwritable` + remediation). The image supports the strict flags — the drop cleanly no-ops when already non-root. This is an explicit product decision (Ben, 2026-09-02): **adoption first**, hardening opt-in.

- `AGENTS.md` / docs must not treat the strict non-root posture as a hard rule the default build violates. The default is deliberately relaxed for adoption; the advanced compose documents how to harden.

## Rebuilding the main container

When the user asks to "rebuild the main container" (or equivalently "rebuild the image" / "restart with the latest code"), the canonical command is:

```bash
cd /opt/tiller-router && docker compose down && docker compose up --build
```

- Always `down` before `up --build` so a stale container isn't left holding the old image's anonymous volumes / healthcheck state, and so the rebuild actually replaces the running process.
- Run from `/home/ben/projects/tiller-router/` (the deployed repo location — this is the dev branch, not a separate `/opt/tiller-router/` checkout), unless the user says otherwise.
- `--build` is required — without it, Compose reuses the existing image and the user's "rebuild" intent isn't honored.
- Do not invent a `docker build` + manual `docker run` workflow unless the user explicitly asks for one. The Compose service is the supported path.

## Toolchain — Go runs in Docker, never on the host

- Go is intentionally **not** installed on the host. `go` is not on PATH and `go: command not found` is expected, not an error. Do not install Go on the host and do not treat the missing host Go as a problem to fix.
- **Use the wrapper `./tiller-go.sh` for ALL Go commands** (build, test, vet, mod tidy, etc.). It runs the pinned `golang:1.26.7-alpine` image with persistent bind-mounted caches and a RAM cap, so repeated runs are fast and cannot OOM the host:
  ```bash
  ./tiller-go.sh test ./...     # run tests (cached, fast)
  ./tiller-go.sh vet ./...
  ./tiller-go.sh mod tidy
  ./tiller-go.sh build ./...
  ```
- **Why the wrapper exists:** the raw `docker run --rm golang:1.26.7-alpine go test ./...` one-liner is stateless — every run re-downloads all modules and cold-recompiles, spiking RAM and getting OOM-killed on this 4GiB box. `tiller-go.sh` fixes both:
  - **Bind-mounted caches** (NOT named volumes — matches the deployment rule): `~/.cache/tiller-go/mod` → `/go/pkg/mod`, `~/.cache/tiller-go/build` → `/root/.cache/go-build`. First run is slow (cold), every later run reuses the cache.
  - **RAM cap:** `--memory=1g` (override: `TILLER_GO_MEM=2g`) + `GOFLAGS=-p=2` (parallelism cap). The cap bounds the container; `-p=2` cuts peak compiler RAM.
  - Image pinned to `golang:1.26.7-alpine` — the same image the Dockerfile build stage uses.
- Do NOT hand-write `docker run --rm -v "$PWD:/src" ... golang:1.26.7-alpine go ...` — use the wrapper so caches and the RAM cap are always applied. If a script or tool invokes bare `go` on the host, that is the bug — point it at `./tiller-go.sh` instead.
- The integration/browser/compatibility tests are fully containerized and need no host Go at all — see `tests/compatibility/run.sh`, `tests/runtime-readonly.sh`, and `tests/browser/run.sh`.

## Security guardrails

- Never re-display, log, or expose provider credentials or client API keys in plaintext after creation — including in error messages, stack traces, and debug output.
- Client API keys are hash-only at rest, using a memory-hard KDF (argon2id preferred). Never swap in a fast hash (SHA-256, MD5, etc.) for "simplicity" or test convenience — including in tests, unless the test explicitly mocks the hashing layer.
- Never log prompt or response bodies, tool arguments, or reasoning content — not even at debug/trace level, not even temporarily "to help debug."
- Backup/export files contain recoverable provider credentials until credential encryption at rest ships (deferred). Any code that touches export/download must not weaken or bypass the admin-auth gate on that endpoint.
- Treat any new admin-facing endpoint as requiring authentication by default. If you're unsure whether a new route needs auth, it needs auth.

## Behavioral guardrails for routing logic

- Ordered fallback is allowed only for an explicitly configured virtual model and only before client-visible output begins. It must follow the stored target order and remain visible in Activity; every upstream non-2xx/read/connect failure is eligible unless the router itself failed or the client request was cancelled/expired. Direct real-model requests, hidden health-based rerouting, retries of the same target, and post-output stream splicing remain forbidden.
- Never let a provider-group feeder setting (`new_models_default`) retroactively touch existing per-model permissions. That distinction is load-bearing — treat any code path that blurs it as a bug.
- Preserve the real/virtual model permission boundary exactly: a client must never be able to reach a model it isn't permitted for, even if it can guess or infer the identifier.

## When to stop and ask instead of proceeding

- The request would add a brand-new dependency, service, or infrastructure component that the human has not explicitly named.
- The request would change client-facing model IDs, provider names, or virtual model names (renames are breaking — confirm intent before touching).
- The request touches credential handling, auth, or logging in a way not explicitly covered by the security guardrails above.
- You find an actual inconsistency between the docs and the current code — report it, don't resolve it silently.

## Branching and commits

- After completing a change, ask the human whether to commit on the current branch (and
  handle the branch/PR later) or create a new branch and open a PR now.
- For small, low-risk changes that are part of a larger in-progress task, committing in
  place is often fine — the branch/PR can come after.
- Do not create a branch or PR unless the human has indicated one is wanted for this
  change.

## Testing expectations

- Any change to routing, permissions, or auth should be verified against existing tests (unit, browser, or compatibility — pick the smallest tier that would catch a regression), not just against a new test you wrote for the change.

### How to test — pick the right route (default by change size)

Run the **minimum** tier that matches the change. Do **not** default to the full suite — the heavy tiers are slow. The test packages (all with warm Docker caches):

| Tier | Command | Approx time | What it verifies |
|---|---|---|---|
| Go unit/integration | `./tiller-go.sh test ./...` | ~30–60s | Backend logic (auth, config, db, providers, server handlers) |
| Go vet | `./tiller-go.sh vet ./...` | seconds | Static analysis |
| Browser / UX | `./tests/browser/run.sh` | ~1¼ min | Admin UI: login, mobile cards, permissions, activity (16 Playwright tests) |
| Compatibility probes | `./tests/compatibility/run.sh` | ~2–4 min | Real OpenAI/Anthropic SDKs + Codex/OpenCode/Claude-Code CLI + Hermes agent + router restart |
| Runtime read-only / security | `./tests/runtime-readonly.sh` | ~30–60s | Read-only rootfs, caps-drop, backup export under deployment settings |

**Sensible default by change type:**

- **Minor UX change** (copy, spacing, a label, a CSS tweak, purely presentational markup): **no tests required.** Just confirm the page still renders (sanity) — run the browser suite only if you changed interactive behaviour (handlers, dialogs, navigation).
- **Minor function change** (small backend/behavioural fix): run **`./tiller-go.sh test ./...`** (and `vet` for Go changes) only. Do not run the browser or compatibility suites unless the change touches the admin UI or a routing/protocol/provider path.
- **Major feature change, or a change that spans backend + UI / routing / providers / auth**: run the Go tests **and** the browser suite; add `tests/compatibility/run.sh` if the change affects provider protocols, client-facing catalogues, or model resolution, and `tests/runtime-readonly.sh` if it touches deployment/security (volumes, caps, read-only, backup, auth).
- **Run the full suite only when instructed, or for a significant feature/release.** Otherwise pick the smallest tier that would catch a regression in what you changed.

When a change is purely frontend (`internal/web/assets/**`), the browser suite is the gate; run `./tiller-go.sh test ./...` for sanity but the UI tests are the ones that matter.

### Test log convention (X = summary, Y = detail)

All test runners follow a two-tier logging convention so agents (or humans)
can diagnose failures without re-running:

- **X (Summary)** — always printed to stdout, regardless of pass/fail.
  Includes exit code, elapsed time, and the path to the detailed log. On
  failure, also prints the first error message inline.
- **Y (Detail)** — always written to a per-run log file containing the full
  stdout+stderr. Default location is repo-local and gitignored
  (`tests/logs/`):

| Runner | Y (detail) path |
|---|---|
| `./tiller-go.sh ...` | `tests/logs/tiller-go/<UTC-ts>-go-*.log` |
| `./tests/browser/run.sh` | `tests/logs/<run-id>/run.log`; Playwright traces/screenshots in `tests/logs/<run-id>/playwright-results/` (preserved on failure, auto-removed on success) |
| `./tests/compatibility/run.sh` | `tests/logs/compat/<UTC-ts>-compat.log` |

To inspect a failed browser run's full output and Playwright artifacts
without re-running: read `tests/logs/<run-id>/run.log` and open the matching
`playwright-results/` directory (each Playwright shard writes
`trace.zip` + `error-context.md` per failed test). Each `run_id` is unique
(`tiller-browser-<pid>`), so preserving a run indefinitely is just a matter
of renaming `tests/logs/<run-id>/` aside before the next invocation.
