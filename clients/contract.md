# Gateway client contract — first pass

Production Go and Python clients implement this contract independently and use
the same fixtures. They depend only on public HTTP, never Tiller internals.

## Connection

Explicit profile `tiller` or `openrouter`, API base URL (including `/v1` or
`/api/v1`), API key, injectable HTTP client, finite request/read timeouts and
bounded bodies/events. Reject userinfo, fragments, non-HTTP schemes and credential
control characters. Preserve an optional deployment path prefix. No redirects
by default; never follow catalog navigation off-origin. No automatic retries,
inference failover, tool execution or persistent credentials. Credentials and
request/response bodies must not appear in exceptions/logging/representations.

## Catalog

Tiller GET `{base}/models`; OpenRouter GET `{base}/models/user`. A custom
OpenRouter-compatible deployment may explicitly override the relative catalog
path; never silently fall back from the user-filtered catalog to a public one.
Normalize ID, name, context/output limits, input/output modalities and optional
vision/tools/reasoning/structured-output flags. Tiller publishes numeric 0/1
flags; accept boolean flags too. Explicit flags take precedence. OpenRouter's
present `supported_parameters` and modality lists may describe support according
to that profile's contract; absent/null data stays unknown. Normalize optional
reasoning effort list (absent/unknown vs null/unrestricted vs []/no values),
budget bounds, toggle/modes/defaults/mandatory. Preserve raw/native metadata.
Never infer behavior from IDs or query models.dev. Return empty successful
catalogs as empty. Filter requirements accept only known positive support;
unknown context cannot satisfy a positive minimum. Cache only in memory per
client/connection; forced refresh errors remain errors and retain the previous
successful snapshot with its original timestamp. Credential change must not
reuse an earlier principal's snapshot.

## Chat

POST `{base}/chat/completions` with explicit stream false/true. Request shape is
OpenAI-compatible Chat, with text/image messages, tools, tool-choice, tool-result
messages, response_format, max_tokens, optional temperature/top_p/reasoning and
extra endpoint-native JSON fields. Do not transform or weaken schemas. Extension
keys cannot override explicit core keys or authentication. Inference does not
require catalog refresh first. Return content, tool calls (IDs/name/arguments),
reasoning/opaque native assistant message for safe replay, model/response ID,
finish reason, optional usage and request ID. Native unknown fields survive.
Usage absent is unknown, not zero. Local schema validation is the host's job.

## Streaming

Incremental SSE parser handles LF/CRLF, comments, multi-line data, fragmented
reads and Unicode. Bound lines/events/body or accumulated tool arguments. Yield
ordered text/reasoning/tool fragments, finish/usage events and exactly one
completion after `[DONE]`. A finish_reason before usage is not stream completion;
OpenRouter may repeat finish_reason in its final accounting chunk. Top-level
error or finish_reason `error`, malformed JSON, premature EOF, timeout and
cancellation fail loudly even with HTTP 200. Empty role/usage frames are not
text. Empty `choices` usage chunks are valid. Preserve native frames and argument
fragments; never parse each tool fragment as complete JSON or execute tools.
Closing the stream closes upstream. Go uses context cancellation; Python owns
stream context management and close. SDK closes only HTTP clients it creates.

## Error categories

invalid_request, authentication, authorization, model_unavailable, rate_limit,
context_limit, unsupported_feature, transport, timeout, http, stream,
malformed_response, cancelled; also billing for HTTP 402. Classify using status
and safe code/type, not just prose. Public errors contain a fixed safe message,
status, bounded code/type and request ID; do not return arbitrary provider prose
that could echo input. Expose Retry-After metadata without automatic retries.

## Fixtures and live acceptance

Both clients consume `clients/fixtures/*.json` and `*.sse`; no real keys or
application data belong there. Unit tests use mock HTTP endpoints. Live checks
must connect to an actual Tiller public interface and invoke `virtual/free` for
catalog, non-streaming text, streaming text, and supported structured/tool/image
features. Do not claim unsupported free-model features were tested successfully.
Live scripts print only counts/status/usage, never raw replies or credentials.

Responses/Messages native operations and async Python are later extensions, not
part of this Chat-first implementation.
