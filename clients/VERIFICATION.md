# Gateway client verification — 2026-09-30

## Automated checks

- `bash clients/test.sh --live tests/logs/gateway-live.json` — PASS. The local
  credential file is gitignored, protected, and removed after testing.
- `./tiller-go.sh test -count=1 ./...` — PASS for every Go package, including
  the new public clients and Tiller HTTP integration tests.
- `./tests/scripts/check-fmt.sh` — PASS.
- `./tiller-go.sh vet ./...` — PASS.
- Native Python **installed wheel**, Python 3.13: 40 unittest cases PASS.
- Python 3.11 installed-package compatibility: unittest cases PASS.

The repeatable client runner tests shared fixtures and malformed/error cases,
builds/installs the Python wheel into a capped disposable container, and tests
the Go client through the actual Tiller HTTP handler with normal authentication,
client permissions, model discovery, a virtual route and a synthetic upstream.
Those Tiller integration tests also cover native image/schema/tool fields,
tool-result continuation, streamed tool fragments/usage and cancellation.

## Real deployment and upstream

Both Go and Python independently called the existing live Tiller deployment
over its public HTTP interface and real configured upstreams. The deployment's
existing free virtual route is named **`tiller/free`**. A temporary Single client
key exposed that existing virtual route as **`virtual/free`**, without changing
the route or its targets. The key was scoped only to that binding and revoked
after acceptance checks.

Both clients passed:

- Authenticated catalog: one client-visible model, `virtual/free`.
- Non-streaming Chat: a nonempty assistant response, virtual model identity,
  provider token usage and no raw response logging.
- Streaming Chat: nonempty text, a clean `[DONE]` completion, and usage.
- JSON-object request: parsed JSON response through the same free route.

The live route reported tools/reasoning as **unknown**, vision as **unsupported**,
and schema structured output as **unsupported**. Capability-gated live
tools/schema/image checks were therefore **skipped**, not reported as successful.
Those request/response shapes are covered by shared fixture tests and the
Tiller public-HTTP integration tests with a controlled upstream.

OpenRouter profile catalog and streaming semantics are tested with its public
contract fixtures and mock HTTP; no paid/live OpenRouter calls were made.

## Scope of this result

This verifies the Chat-first client delivery, not native Responses/Messages or
downstream Billbot/Olympus migrations. Tiller production provider/routing/OAuth
code was not modified. The main module import path and remote publication remain
separate release work; use local source/wheel adoption until publication.
