# Thin gateway clients

Production **Chat-first Go and Python clients**, hosted together but dependent
only on public HTTP. Choose a profile, API base URL and client key; fetch a
capability-aware catalog or call a configured model immediately. Tiller handles
its upstream providers, routing, OAuth and translation. OpenRouter is an
alternative peer gateway, not a dependency of Tiller.

## Layout

- [`go/`](go/) — standard-library Go package `gateway` in the main Go module.
- [`python/`](python/) — installable native `tiller_gateway` package, Python
  3.11+, synchronous `httpx` transport.
- [`contract.md`](contract.md) — shared behavioral contract.
- [`fixtures/`](fixtures/) — synthetic shared catalog/result/stream fixtures.
- [`examples/`](examples/) — Go modes and native Python examples, including a
  Billbot-shaped bounded tool loop and real-Tiller acceptance probes.
- [`test.sh`](test.sh) — containerized fixture, installed-wheel, and Tiller
  public-HTTP integration checks; optional explicit live-provider checks.

## First-pass surface

Both implementations provide:

- Explicit `tiller` / `openrouter` profiles; deployment path prefixes preserved.
- Tiller's `/models` and OpenRouter's user-filtered `/models/user`; custom
  compatible deployments can override a **relative** catalog path.
- Context/output limits, modalities, tools/vision/reasoning/schema flags and
  reasoning effort/budget/default metadata. Missing capability data stays
  unknown. Required capabilities match only known-positive support.
- Last-successful in-memory catalog caching (five-minute default), explicit
  refresh, retained snapshots on failure, and filters. No models.dev fetches.
- Chat requests with native text/image messages, tools, tool results, JSON
  formats, reasoning and extra endpoint-native body fields.
- Native assistant messages and raw result/frame access for safe continuation.
- Streaming text/reasoning/tool fragments, finish state, usage and terminal
  completion; HTTP-200 stream errors and premature EOF fail loudly.
- Usage/cache counters, response/request IDs, safe categorized errors and
  bounded request/response processing. No automatic retries or redirects.

`structured_output` describes published schema support; `json_object` describes
published JSON mode support separately. A catalog flag is not a guarantee that
every route or upstream will accept every request option. Tiller presently
omits some stored metadata, including modalities, from its client catalog;
the clients preserve that uncertainty and do not call admin endpoints to fill
it. Inference need not fetch a catalog first.

Native Responses/Messages operations, async Python, direct upstream provider
adapters, OAuth, automatic tool execution and persistent caches are later scope.
The gateway remains responsible for converting a Chat request to its chosen
upstream protocol. The SDK never adds model/provider heuristics or silently
weakens a schema.

## Go: local adoption

```go
import gateway "github.com/tiller-router/tiller-router/clients/go"

client, err := gateway.New(gateway.Config{
    Profile: gateway.Tiller,
    BaseURL: "http://localhost:8080/v1",
    APIKey: keyFromYourSecretStore,
})
if err != nil { return err }
defer client.Close()

models, err := client.ListModels(ctx)
if err != nil { return err }
eligible := gateway.FilterModels(models, gateway.Requirements{Tools: true})
_ = eligible // the application selects a model, not the SDK

reply, err := client.Chat(ctx, gateway.Request{
    Model: selectedModel,
    Messages: []gateway.Message{{"role": "user", "content": "Hello"}},
})
```

For another checkout before publication, use a temporary local `go.mod`
replacement for `github.com/tiller-router/tiller-router` pointing to this
checkout. The main module currently requires Go 1.26.7. A local import is tested;
`@latest` publication under the current module path remains a separate release
decision. See [`go/README.md`](go/README.md).

## Python: local adoption

Install `clients/python` into the consuming application's container or project
virtual environment, or build a wheel and pin that artifact:

```bash
python -m pip install /path/to/tiller-router/repo/clients/python
```

```python
from tiller_gateway import GatewayClient, ModelFilter

with GatewayClient("tiller", "http://localhost:8080/v1", key_from_secret_store) as client:
    models = client.list_models(ModelFilter(tools=True))
    reply = client.chat(selected_model, [{"role": "user", "content": "Hello"}])
    # Inspect reply.content/tool_calls/usage; do not log reply.raw as diagnostics.
```

For Billbot, adapt its existing `chat(...)` seam to `GatewayClient.chat` and
preserve its Pydantic gate, tenant-scoped read tools and classifier budget. For
Olympus, the Go client can replace model-ID-only discovery while Hermes still
executes worker turns. No consuming-project changes ship here.

## Streaming and tool continuation

Go uses `stream.Next() (Event, error)`; Python uses a context-managed iterator.
They expose equivalent information, not identical method names. Go additionally
yields a native `frame` before typed events for every frame; Python yields a
`frame` only when the native frame has no typed events. Do not compare total
event counts across languages. Compare typed text/tool/usage and terminal state.

Tool arguments are **partial JSON until the call completes**. An application
assembles/validates them, authorizes the function, executes it and appends the
native assistant message plus correlated tool-result message. The examples
demonstrate a bounded loop; the SDK itself never calls host functions. Finish
may repeat in a final accounting frame. Only `[DONE]` completes a Chat stream.

Go `Config.Timeout` is an overall per-request deadline. Python's `timeout` is
httpx's finite connect/read/write/pool timeout (stream read inactivity), not an
overall deadline. Both close upstream on early stream close; injected HTTP
clients remain caller-owned. Cache entries are per client/principal; construct
a new client after changing a key or endpoint.

## Run tests

From `repo/`:

```bash
bash clients/test.sh
```

The runner uses `tiller-go.sh` for Go, a capped Python test container and an
installed wheel. Integration tests start the actual Tiller HTTP handler with
normal admin/client authentication, permissions and virtual routes, backed by
a synthetic upstream. No tests import Tiller internals into either client.

To additionally call an **existing real Tiller instance and real upstream**,
create a permission-scoped client key in Tiller and a protected `0600` JSON file
under gitignored `tests/logs/`:

```json
{"base_url":"http://172.17.0.1:8080/v1","api_key":"<client key>","model":"virtual/free"}
```

`172.17.0.1` is the host gateway on this Linux Docker setup; use a URL reachable
from the test containers. If the existing virtual route has a different name,
a temporary **Single** key can expose `virtual/free` without renaming the route.
Remove/revoke that key and protected file after testing.

```bash
bash clients/test.sh --live tests/logs/gateway-live.json
```

Live probes perform catalog, complete Chat, streaming and JSON-object calls;
optional tools/schema/image probes run only for known-positive advertised
support. They print statuses/counts/usage, never keys or model reply bodies.
Provider refusal, rate limits or unavailable upstreams remain test failures,
not synthesized success. These calls use upstream resources; live execution is
explicitly opt-in. OpenRouter-profile behavior is fixture-tested; live Tiller
testing is recorded separately in [`VERIFICATION.md`](VERIFICATION.md).

## Security and license

The client stores the key only in its private connection state and request
Authorization header. Catalogs contain no key; errors suppress provider prose
and original transport exception text. Raw messages, tool arguments, reasoning
and images are deliberately accessible as application data, not safe logs.
Host-supplied HTTP hooks and custom transports are host policy. No redirected
credentials, cross-origin catalog traversal or automatic remote image fetching.

Go and Python clients inherit **AGPL-3.0**. The Python wheel includes its license;
httpx is BSD-3-Clause. Release strategy is independent from this code delivery.
