# Go gateway client

Package `gateway`, import path
`github.com/tiller-router/tiller-router/clients/go`. Standard library only;
does not import Tiller's server/store/provider internals. It stays in the main
module (currently Go 1.26.7). Until the remote module path is published, test
downstream imports through a temporary local `replace` directive.

## API

- `New(Config) (*Client, error)`, `Close()` — one immutable endpoint/profile/key.
- `ListModels(ctx)` — five-minute cache by default; failure after expiry raises.
- `RefreshModels(ctx)` — forced refresh; failure retains old cache/time.
- `CatalogCachedAt()` — original successful fetch timestamp.
- `CachedModels()` — isolated last-success models/time without a fetch, including
  after refresh failure. A zero timestamp means no snapshot yet.
- `FilterModels(models, Requirements)` — known-positive capabilities, min
  context/output and input/output modalities. Support enums are `Unknown`,
  `Unsupported`, `Supported`; do not treat numeric enum values as booleans.
- `Chat(ctx, Request)` — native Chat inputs, `Result.Choices`, optional usage.
- `Stream(ctx, Request)` — `Next() (Event, error)`, terminal `CompletionEvent`,
  then `io.EOF`. Close abandoned streams. Context cancellation closes upstream.

`Message` is a native map: retain `Choice.Message` for assistant replay, then
append the matching `role:tool`, `tool_call_id` and serialized result. `Raw` and
`Choice.Native` preserve optional response fields. `Request.Extra` preserves
endpoint extensions but rejects conflicting core/auth keys. Schemas and image
URLs pass through without automatic mutation or fetching.

`Usage` preserves nil/unreported prompt/completion/total and cache-read/write
counts plus native accounting. SDK `Error` exposes fixed safe category/status,
allowlisted code/type, request ID and Retry-After; no request/key/raw prose.

## Transport and limits

`Config.BaseURL` includes `/v1` or `/api/v1`; deployment prefixes are preserved.
Explicit profiles `Tiller` or `OpenRouter` select catalog semantics, never the
key shape. `CatalogPath` optionally overrides a relative catalog endpoint.
`HTTPClient` permits custom host network policy; it is shallow-cloned without
altering the original, redirects are disabled, and the caller owns its idle
connections. `Close()` cancels active requests and prevents future calls.

`Timeout` covers the whole request, including stream reads; default 60 seconds.
Default bounds: total body 8 MiB, SSE line 1 MiB, event 2 MiB, accumulated tool
arguments 1 MiB, catalog pages 100. Zero config limits select defaults; use
`RefreshModels` to bypass cache. No retries, tool execution or gateway fallback.

## Runnable examples

From `repo/`, provide environment variables `GATEWAY_BASE_URL`,
`GATEWAY_API_KEY`, `GATEWAY_MODEL`, optionally `GATEWAY_PROFILE=openrouter`.
The container wrapper deliberately does not forward arbitrary environment;
use the example's protected `--config /src/tests/logs/connection.json` input
when running through it.

```bash
./tiller-go.sh run ./clients/examples/go --config /src/tests/logs/connection.json --mode catalog
./tiller-go.sh run ./clients/examples/go --config /src/tests/logs/connection.json --mode basic
./tiller-go.sh run ./clients/examples/go --config /src/tests/logs/connection.json --mode stream
```

Modes `tools`, `image`, `structured` demonstrate capability-gated native
requests. `live` runs catalog, Chat, stream and JSON-object checks; add
`--features tools,structured,image` for advertised-positive features. Summaries
contain no response bodies or credentials. See [`../README.md`](../README.md)
for the combined containerized test runner and deployment requirements.
