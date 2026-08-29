# AGENTS.md — Tiller Router

Guardrails for any coding agent working in this repository. This file does not restate the spec — it tells you how to behave around it.

## Source of truth

- `tiller-router-v1-specification.md` is the **frozen** V1 spec. Frozen means frozen: do not add, soften, or reinterpret requirements in it. If something seems missing or wrong, stop and flag it — don't silently patch the spec or work around it in code.
- `tiller-router-roadmap.md` describes deferred work. Nothing in it is authorized for implementation unless a human explicitly asks for that specific roadmap item by name.
- If the two documents conflict, or if a request conflicts with either, stop and ask rather than picking one.

## Scope discipline

- Before writing code for a new feature, ask: is this in the V1 spec's functional scope (§3), or is it in §27 Non-Goals / roadmap §15 Anti-Roadmap? If it's a non-goal, do not implement it, even as a "small" version, even if it seems like it would obviously help, and even if it's technically easy. Say so and stop.
- Do not add a dependency, service, or infrastructure component that isn't already implied by the spec (no Redis, Postgres, message queues, vector DBs, Kubernetes, etc.) without explicit sign-off.
- Do not "clean up" the roadmap's phase ordering or scope on your own initiative. Roadmap sequencing is a human decision.
- If a task requires touching something explicitly marked deferred (e.g. credential encryption, fallback routing) to complete the immediate ask, stop and surface that instead of quietly building the deferred piece too.

## Deployment model — non-negotiable

- Everything runs as a single Docker Compose service under `/opt/tiller-router/`.
- Bind mounts only. **Never** introduce a Docker named volume.
- All persistent state lives under `./data`. If you add any new persistent file, it goes under `./data` and must survive a container restart and a directory move to a different host with no other changes.
- No Kubernetes artifacts of any kind (manifests, Helm, operators). This is Compose-only.
- Don't add anything that requires a host-published port when a reverse-proxy Docker network is in use.
- Approved deviation (2026-08-29, testing): the base `docker-compose.yml` publishes `0.0.0.0:${TILLER_TEST_PORT:-8080}:8080` for LAN access during testing. This is a deliberate, human-approved exception to the "base publishes no host port" design in README §31. Do not silently remove it, and do not extend it to other ports or to `0.0.0.0` exposure in the proxy override.
- Don't require Docker socket access or privileged mode.

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

- The request conflicts with the frozen spec.
- The request would require an anti-roadmap item (Redis, Postgres, Kubernetes, MCP, vector DB, semantic caching, built-in secret manager, etc.) even indirectly.
- The request would change client-facing model IDs, provider names, or virtual model names (renames are breaking — confirm intent before touching).
- The request touches credential handling, auth, or logging in a way not explicitly covered above.
- You find an actual inconsistency between the spec and the roadmap, or between either document and the current code — report it, don't resolve it unilaterally.

## Testing expectations

- Any change to routing, permissions, or auth should be checked against the relevant V1 Acceptance Test in §28 before being considered done, not just against a new unit test you wrote for the change.
- §28.4 (virtual routing hides the real target) and §28.5 (immediate remap, no restart) are the two tests most likely to silently regress — re-verify both after any change to virtual model resolution or provider/model mapping.
