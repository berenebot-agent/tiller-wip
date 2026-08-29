# Client configuration

Replace `https://router.example.com` with the reverse-proxy URL, `virtual/coding`
with a model visible to the client key, and every placeholder secret with a
one-time Tiller client key.

The base URL differs by SDK convention:

- OpenAI-compatible clients normally use `https://router.example.com/v1`.
- Anthropic clients normally use `https://router.example.com` because the SDK
  appends `/v1/messages`.

## Hermes Agent — primary release gate

Current Hermes supports `chat_completions`, `codex_responses`, and
`anthropic_messages`. Declare the transport explicitly so URL heuristics cannot
select the wrong wire format.

Store the client secret in `~/.hermes/.env`:

```dotenv
TILLER_ROUTER_KEY=sk-tr-REPLACE_ONCE
```

Define one or more named custom providers in `~/.hermes/config.yaml`.

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

Select the named provider with `hermes model`. Hermes documents these transport
values and named-provider fields in its
[provider guide](https://hermes-agent.nousresearch.com/docs/integrations/providers/).

## OpenCode

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

OpenCode's current provider configuration uses `options.baseURL` and supports
the OpenAI-compatible AI SDK package; see its
[provider documentation](https://opencode.ai/docs/providers).

## Codex CLI

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

Declare `wire_api = "responses"` explicitly. The current Codex schema defines
custom provider `base_url`, `env_key`, and Responses wire configuration in the
[official Codex configuration schema](https://github.com/openai/codex/blob/main/codex-rs/core/config.schema.json).

Native Responses requests may use provider stateful fields only when the
resolved upstream itself declares native Responses support. Cross-protocol
translation rejects conversations, previous-response state, storage, files,
background mode, MCP, and provider-hosted tools with `unsupported_feature`.

## Claude Code

```sh
export ANTHROPIC_BASE_URL='https://router.example.com'
export ANTHROPIC_AUTH_TOKEN='sk-tr-REPLACE_ONCE'
export ANTHROPIC_MODEL='virtual/coding'
claude
```

Tiller accepts both `Authorization: Bearer` and `x-api-key` on `/v1/messages`.
Anthropic's [gateway documentation](https://code.claude.com/docs/en/llm-gateway)
notes that it supports gateways exposing the expected Anthropic format but
does not support routing Claude Code to non-Claude models; use an Anthropic-
compatible target for supported Claude Code deployments.

## OpenAI SDK

Python:

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

Chat Completions use the same client and `client.chat.completions.create(...)`.

## Anthropic SDK

Python:

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

## cURL probes

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
