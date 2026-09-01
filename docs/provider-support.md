# Provider support

This page describes the provider *adapters* shipped in Tiller Router
`v0.1.0-alpha.1`. “Automated contract” means the adapter, endpoint
construction, authentication headers, discovery parser, or protocol translation
is covered by local tests and mock upstreams. It does not mean that every
external provider account, model, quota, feature, or regional endpoint has been
live-tested.

There are no provider credentials in the repository and no continuous live API
smoke test. Check the provider's current API documentation and run a safe
request against your own account before production use.

## Status legend

- **Verified (mocked):** covered by automated contract or server tests.
- **Compatibility / best effort:** uses the provider's documented OpenAI-style
  or native shape, with shared adapter coverage; provider-specific differences
  may require a custom base URL or model settings.
- **Experimental:** integration is present, but discovery or protocol behavior
  is provider-specific and has not had broad external validation.

## Provider matrix

| Type | Protocols | Discovery | Credential | Status and limits |
| --- | --- | --- | --- | --- |
| `generic-openai` | Chat | OpenAI-compatible | Optional | **Verified (mocked)**; intended for OpenAI-compatible servers. |
| `openai` | Chat, Responses | OpenAI | Required | **Compatibility / best effort**; native endpoint and auth paths are covered by adapter tests, not a live account. |
| `anthropic` | Messages | Anthropic | Required | **Compatibility / best effort**; native headers and shared translation are covered with mocks. |
| `openrouter` | Chat | OpenAI-compatible | Required | **Compatibility / best effort**; model discovery and shared Chat path are mocked. |
| `ollama-local` | Chat | Ollama | None | **Experimental**; default points at the Docker host and requires local network reachability. |
| `ollama-cloud` | Chat | Ollama | Required | **Experimental**; cloud availability, model features, and quotas are not live-tested. |
| `deepseek`, `zai`, `gemini` | Chat | OpenAI-compatible | Required | **Compatibility / best effort**; shared Chat adapter and provider-specific base URL. |
| `azure-openai` | Chat, Responses | OpenAI-compatible | Required | **Compatibility / best effort**; supply the deployment-specific base URL and query/auth settings. |
| `bedrock-api-key` | Chat | OpenAI-compatible | Required | **Experimental**; base URL and API-key mode are configurable; AWS-native signing is not provided by this type. |
| `groq`, `mistral`, `xai`, `together` | Chat | OpenAI-compatible | Required | **Compatibility / best effort**; shared OpenAI-compatible adapter. |
| `fireworks`, `cerebras`, `perplexity`, `nvidia-nim` | Chat | OpenAI-compatible | Required | **Compatibility / best effort**; shared adapter, with provider/model feature differences possible. |
| `huggingface` | Chat | Hugging Face | Required | **Experimental**; discovery and model naming depend on Hugging Face availability. |
| `cloudflare-ai` | Chat | Cloudflare | Required | **Experimental**; configure the account-specific base URL and model route. |
| `alibaba-qwen`, `minimax` | Chat | OpenAI-compatible | Required | **Compatibility / best effort**; shared adapter, region and model availability vary. |
| `opencode-zen` | Chat, Responses, Messages | OpenCode | Required | **Experimental**; native protocol selection is covered by mocks; service policy and model access can change. |
| `opencode-go` | Chat, Responses, Messages | OpenCode | Required | **Experimental**; same caveat as Zen, with Go service/model availability subject to change. |
| `vllm` | Chat | OpenAI-compatible | Optional | **Compatibility / best effort**; self-hosted deployment and model capabilities are operator-managed. |
| `lm-studio` | Chat | OpenAI-compatible | Optional | **Compatibility / best effort**; default assumes a reachable host-side LM Studio server. |
| `llama-cpp` | Chat | OpenAI-compatible | Optional | **Compatibility / best effort**; default assumes a reachable host-side llama.cpp server. |

## What is covered

The repository tests provider descriptor registration, URL construction and
query preservation, authentication header selection, paged discovery, selected
capability extraction, native OpenCode protocol selection, request translation,
ordered fallback, and SDK/CLI compatibility against local mock services. The
tests do not certify provider uptime, billing, rate limits, policy behavior,
tool support, context limits, or the accuracy of a provider's model catalogue.

When a provider does not implement a requested protocol or feature, Tiller
returns a clear error rather than silently selecting a different provider or
model. Report provider-specific failures with the provider type, sanitized
status/error, and model identifier; never include credentials or request/response
bodies.
