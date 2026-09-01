# AGENTS.md — Tiller Router

Guardrails for any coding agent working in this repository. This file does not restate the spec — it tells you how to behave around it.

## Source of truth

- `tiller-router-v1-specification.md` is the reference spec. It is **no longer frozen**: in live dev it may be amended with explicit human sign-off, and diverging implementation (a planned change, not an accident) is acceptable when the human is driving it.
- `tiller-router-roadmap-v2-core.md` describes deferred core work (active phases plus a Deferred Backlog). `tiller-router-roadmap-saas-multiuser.md` describes deferred multi-user/SaaS work. These are a backlog of ideas, not commitments.
- If the two documents conflict, or a request conflicts with an approved change, ask rather than silently picking one — but do not treat the spec/roadmap as an impassable wall in live dev.

## Scope discipline

- Before writing code for a new feature, check whether it's in the V1 spec's functional scope (§3), in §27 Non-Goals, or in the roadmaps' anti-roadmap. A non-goal or roadmap item is fine to build in live dev **with explicit human sign-off**, but never silently and never "just to see."
- Adding a brand-new dependency, service, or infrastructure component (Redis, Postgres, message queues, vector DBs, Kubernetes, etc.) still requires an explicit, named request from a human.
- Do not "clean up" the roadmap's phase ordering or scope on your own initiative. Roadmap sequencing is a human decision.
- If a task requires touching something explicitly marked deferred (e.g. credential encryption, fallback routing) to complete the immediate ask, surface it and get sign-off rather than quietly building the deferred piece too.

## Deployment model — non-negotiable

- Everything runs as a single Docker Compose service under `/opt/tiller-router/`.
- Bind mounts only. **Never** introduce a Docker named volume.
- All persistent state lives under `./data`. If you add any new persistent file, it goes under `./data` and must survive a container restart and a directory move to a different host with no other changes.
- No Kubernetes artifacts of any kind (manifests, Helm, operators). This is Compose-only.
- Don't add anything that requires a host-published port when a reverse-proxy Docker network is in use.
- Don't require Docker socket access or privileged mode.

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
- Backup/export files contain recoverable provider credentials until credential encryption at rest ships (roadmap). Any code that touches export/download must not weaken or bypass the admin-auth gate on that endpoint.
- Treat any new admin-facing endpoint as requiring authentication by default. If you're unsure whether a new route needs auth, it needs auth.

## Behavioral guardrails for routing logic

- Never implement silent fallback, silent retry, or silent re-routing to a different model/provider than the one resolved. If a target is broken, fail clearly (per §21) — don't make the router "helpful" by picking something else.
- Never let a provider-group feeder setting (`new_models_default`) retroactively touch existing per-model permissions. That distinction is load-bearing throughout the spec — treat any code path that blurs it as a bug.
- Preserve the real/virtual model permission boundary described in the spec exactly: a client must never be able to reach a model it isn't permitted for, even if it can guess or infer the identifier.

## When to stop and ask instead of proceeding

- The request would add a brand-new dependency, service, or infrastructure component that the human has not explicitly named.
- The request would change client-facing model IDs, provider names, or virtual model names (renames are breaking — confirm intent before touching).
- The request touches credential handling, auth, or logging in a way not explicitly covered by the security guardrails above.
- You find an actual inconsistency between the spec and the roadmap, or between either document and the current code — report it, don't resolve it silently.

## Testing expectations

- Any change to routing, permissions, or auth should be checked against the relevant V1 Acceptance Test in §28 before being considered done, not just against a new unit test you wrote for the change.
- §28.4 (virtual routing hides the real target) and §28.5 (immediate remap, no restart) are the two tests most likely to silently regress — re-verify both after any change to virtual model resolution or provider/model mapping.

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
