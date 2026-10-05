# tiller_gateway

A native, synchronous Python Chat-first SDK for Tiller and OpenRouter-compatible
gateways. Python 3.11+ is supported; `httpx>=0.28,<1` is the only direct runtime
dependency. The package is licensed **AGPL-3.0-only**; see `LICENSE`.

## Installation

Build a local wheel from the repository root in a disposable container:

```bash
docker run --rm --memory=512m --memory-swap=512m \
  -v "$PWD/clients/python:/source:ro" -w /tmp python:3.13-slim \
  sh -c 'cp -R /source /tmp/package && python -m pip wheel --no-deps --no-cache-dir /tmp/package -w /tmp/wheels && python -m pip install /tmp/wheels/tiller_gateway-*.whl && python -c "import tiller_gateway; print(tiller_gateway.GatewayClient.__name__)"'
```

For an existing application virtual environment, install the wheel produced by
your build pipeline with `python -m pip install /path/to/tiller_gateway-0.1.0-py3-none-any.whl`.
Alternatively, install this local project into that environment with
`python -m pip install /path/to/repo/clients/python`. Do not install packages into
the host system Python. Build tools such as setuptools are not runtime dependencies.

## Connection and basic Chat

```python
import os
from tiller_gateway import GatewayClient

with GatewayClient(
    profile="tiller",
    base_url=os.environ["TILLER_BASE_URL"],
    api_key=os.environ["TILLER_API_KEY"],
    timeout=60.0,
    cache_ttl=300.0,
) as client:
    reply = client.chat(
        os.environ["TILLER_MODEL"],
        [{"role": "user", "content": "Reply with exactly OK"}],
        max_tokens=128,
        temperature=0,
    )
    print({"status": "received", "tool_calls": len(reply.tool_calls)})
```

Supply the API base, including `/v1` or `/api/v1`; deployment prefixes are
preserved. Profiles must be explicitly `tiller` or `openrouter`. OpenRouter uses
the user-filtered `models/user` catalog, never a silent public-catalog fallback.
`catalog_path` can override the relative catalog endpoint; queries, control
characters, absolute paths and escaped dot segments are rejected.

`http_client=` accepts a synchronous `httpx.Client`. The SDK always disables
redirects per request and sends its own Bearer authentication. It closes only
clients it creates, but always closes its own response streams. Injected-client
hooks, logging, custom transports, and their side effects are application-owned;
do not configure hooks that log credentials or request/response bodies. Timeouts
must be finite and positive for connect, read, write and pool operations; they
are httpx per-operation/inactivity timeouts, not an overall stream deadline.

## Catalog and capabilities

`list_models(requirements=None, refresh=False)` returns a tuple of `Model` records.
`refresh_models()` forces discovery and returns `Catalog`. `cached_catalog`
returns an isolated last-success snapshot with its original `fetched_at` Unix
timestamp. Cache reuse lasts 300 seconds by default; `cache_ttl=0` disables reuse.
Expiry/forced-refresh failures raise and retain the previous snapshot, not a
silent stale success. Create a new client for a different connection or principal.

`ModelFilter` supports `vision`, `tools`, `reasoning`, `structured_output`,
`json_object`, `min_context`, `min_output`, and input/output modality requirements.
Flags are `True`, `False`, or `None` (unknown); only known-positive support meets
a requirement. Explicit null/invalid flags remain unknown. No model-ID heuristics
or external metadata discovery are used. `structured_output` means strict schema
support (`structured_outputs`); `json_object` is separate JSON mode support
(`response_format`). Raw metadata is preserved. Reasoning metadata includes
efforts, budget bounds, toggle, defaults and mandatory state: `efforts_known=False`
means unknown; known `efforts=None` means unrestricted; `efforts=()` means no values.

## Native requests, tools and replay

`chat(model, messages, **kwargs)` accepts `tools`, `tool_choice`, `response_format`,
`max_tokens`, `temperature`, `top_p`, `reasoning`, endpoint-native keyword arguments,
and `extra_body`. Schemas, text/image messages and tool results are not rewritten.
Extension keys cannot override core fields (case-insensitive) or authentication.
Chat never requires discovery first, retries automatically, reroutes, or executes
tools. Request schemas and tool argument business validation remain host-owned.

`ChatResult` provides `content`, `tool_calls`, `reasoning`, `assistant_message`,
`model`, `id`, `finish_reason`, optional `usage`, `request_id` and `raw`. Completed
tool calls require function type, string IDs/names and valid JSON-object argument
text. Argument text is preserved; it is never executed. Append the entire
`assistant_message`, not a reconstructed text-only message, before adding
`{"role": "tool", "tool_call_id": call.id, "content": ...}`. This retains opaque
reasoning and provider extensions. The application must authorize, validate and
execute tools, bound loop turns/results, and choose its own error policy.

Usage fields are optional, not implicit zeroes. Cache accounting exposes
`cached_tokens`, `cache_read_input_tokens` and `cache_creation_input_tokens`;
the full native usage remains available as `usage.raw`.

## Streaming and errors

```python
with client.stream(model, messages, max_tokens=128) as stream:
    for event in stream:
        if event.kind == "usage":
            print({"total_tokens": event.usage.total_tokens})
```

Events are `text`, `reasoning`, `tool`, `finish`, `usage`, `frame`, and `done`.
Opaque `reasoning_details`, raw decoded frames and SSE `raw_frame` data survive.
Tool arguments arrive as fragments, not independently parseable JSON. Finish
does not complete a stream: usage can follow, and only `[DONE]` emits `done`.
Early context exit/`close()` closes upstream; further reads report cancellation.
Malformed frames, upstream errors, premature EOF and timeouts fail loudly.
Default `Limits` are body 8 MiB, line 1 MiB, event 2 MiB, accumulated arguments
1 MiB, and 100 catalog pages; catalog pagination cannot leave the origin.

`GatewayError` exposes fixed safe messages and category, status, allowlisted
code/type, redacted request ID and Retry-After metadata. Provider prose and
original transport exceptions are not displayed. The SDK does not log bodies.
Raw records are deliberate application access: never log them as diagnostics.

## Examples and verification

`../examples/python/basic.py` runs catalog, Chat and streaming using environment
configuration and prints summaries only. `billbot_adapter.py` demonstrates image,
schema and bounded auto-choice tool-loop integration without application changes.
`live_acceptance.py` accepts an owner-only JSON file containing `base_url`,
`api_key`, `model` (`virtual/free`); `--features` adds JSON-object baseline plus
known-supported tools, strict schema and image probes using `../fixtures/pixel.png.b64`.
Live probes incur upstream requests and must be explicitly run; they print only
counts/status/usage and do not claim skipped capabilities were verified.

Tests use stdlib unittest, shared fixtures, MockTransport and local mock HTTP:
`python -m unittest discover -s clients/python/tests -v` with the package installed.
Async Python, native Responses/Messages operations and automatic tool execution
are outside this Chat-first API.
