# Client integrations

## Connect your tools

Replace `https://router.example.com` with your Tiller (reverse-proxy) URL, `virtual/coding`
with a model visible to the client key, and every placeholder secret with a
one-time Tiller client key.

The base URL differs by SDK convention:

- OpenAI-compatible clients normally use `https://router.example.com/v1`.
- Anthropic clients normally use `https://router.example.com` because the SDK
  appends `/v1/messages`.

Tiller accepts both `Authorization: Bearer` and `x-api-key` on `/v1/messages`.

### Hermes Agent

Current Hermes supports `chat_completions`, `codex_responses`, and
`anthropic_messages`. Declare the transport explicitly so URL heuristics cannot
select the wrong wire format. Store the client secret in `~/.hermes/.env`:

```dotenv
TILLER_ROUTER_KEY=sk-tr-REPLACE_ONCE
```

Define one or more named custom providers in `~/.hermes/config.yaml`, then select
one with `hermes model` (or `provider: custom:<name>`):

Chat Completions:

```yaml
providers:
  tiller-chat:
    api: https://router.example.com/v1
    key_env: TILLER_ROUTER_KEY
    transport: chat_completions
    default_model: virtual/coding
```

Codex/Responses:

```yaml
providers:
  tiller-responses:
    api: https://router.example.com/v1
    key_env: TILLER_ROUTER_KEY
    transport: codex_responses
    default_model: virtual/coding
```

Anthropic Messages:

```yaml
providers:
  tiller-messages:
    api: https://router.example.com
    key_env: TILLER_ROUTER_KEY
    transport: anthropic_messages
    default_model: virtual/coding
```

### OpenCode

Use a custom OpenAI-compatible provider and list the permitted virtual IDs that
OpenCode should offer:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "tiller": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Tiller Router",
      "options": {
        "baseURL": "https://router.example.com/v1",
        "apiKey": "{env:TILLER_ROUTER_KEY}"
      },
      "models": {
        "virtual/coding": { "name": "Virtual / Coding" },
        "virtual/general": { "name": "Virtual / General" }
      }
    }
  },
  "model": "tiller/virtual/coding"
}
```

### Codex CLI

Set the client secret in the environment and add a Responses provider to
`~/.codex/config.toml`:

```sh
export TILLER_ROUTER_KEY='sk-tr-REPLACE_ONCE'
```

```toml
model = "virtual/coding"
model_provider = "tiller"

[model_providers.tiller]
name = "Tiller Router"
base_url = "https://router.example.com/v1"
env_key = "TILLER_ROUTER_KEY"
wire_api = "responses"
```

Declare `wire_api = "responses"` explicitly. Native Responses requests may use
provider stateful fields only when the resolved upstream itself declares native
Responses support; cross-protocol translation rejects conversations,
previous-response state, storage, files, background mode, MCP, and
provider-hosted tools with `unsupported_feature`.

### Claude Code

```sh
export ANTHROPIC_BASE_URL='https://router.example.com'
export ANTHROPIC_AUTH_TOKEN='sk-tr-REPLACE_ONCE'
export ANTHROPIC_MODEL='virtual/coding'
claude
```

### Python SDKs

OpenAI:

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://router.example.com/v1",
    api_key="sk-tr-REPLACE_ONCE",
)

for event in client.responses.create(
    model="virtual/coding",
    input="Return one sentence.",
    stream=True,
):
    print(event)
```

Anthropic:

```python
from anthropic import Anthropic

client = Anthropic(
    base_url="https://router.example.com",
    api_key="sk-tr-REPLACE_ONCE",
)

message = client.messages.create(
    model="virtual/coding",
    max_tokens=256,
    messages=[{"role": "user", "content": "Return one sentence."}],
)
print(message.content)
```

### cURL probes

Catalogue:

```sh
curl -fsS https://router.example.com/v1/models \
  -H 'Authorization: Bearer sk-tr-REPLACE_ONCE'
```

Streaming Chat Completions:

```sh
curl -N https://router.example.com/v1/chat/completions \
  -H 'Authorization: Bearer sk-tr-REPLACE_ONCE' \
  -H 'Content-Type: application/json' \
  -d '{"model":"virtual/coding","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
```

Anthropic Messages:

```sh
curl -fsS https://router.example.com/v1/messages \
  -H 'x-api-key: sk-tr-REPLACE_ONCE' \
  -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"virtual/coding","max_tokens":128,"messages":[{"role":"user","content":"Hello"}]}'
```

## Reusable application clients

Native [Go and Python gateway clients](../clients/README.md) provide normalized
model/capability catalogs, Chat calls, streaming, tool-result continuation,
structured-output options, usage and safe errors against Tiller or OpenRouter.
They use only public HTTP and ship with runnable examples and shared contract
tests. Applications retain their own tool execution and business policy.
