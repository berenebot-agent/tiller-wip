# Third-party notices

Tiller Router is distributed under the [GNU Affero General Public License,
version 3](LICENSE). The dependencies below are separate works; their own
licenses continue to apply. Go module versions are the direct and transitive
modules listed in this repository's `go.mod`.

## Go modules

| Module | Version | License | Upstream notice |
| --- | --- | --- | --- |
| `golang.org/x/crypto` | v0.55.0 | BSD-3-Clause | [LICENSE](https://github.com/golang/crypto/blob/master/LICENSE) |
| `modernc.org/sqlite` | v1.39.1 | BSD-3-Clause | [LICENSE](https://gitlab.com/cznic/sqlite/-/blob/master/LICENSE) |
| `github.com/dustin/go-humanize` | v1.0.1 | MIT | [LICENSE](https://github.com/dustin/go-humanize/blob/master/LICENSE) |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause | [LICENSE](https://github.com/google/uuid/blob/master/LICENSE) |
| `github.com/mattn/go-isatty` | v0.0.20 | MIT | [LICENSE](https://github.com/mattn/go-isatty/blob/master/LICENSE) |
| `github.com/ncruces/go-strftime` | v0.1.9 | MIT | [LICENSE](https://github.com/ncruces/go-strftime/blob/main/LICENSE) |
| `github.com/remyoudompheng/bigfft` | 24d4a6f8 | BSD-3-Clause | [LICENSE](https://github.com/remyoudompheng/bigfft/blob/master/LICENSE) |
| `golang.org/x/exp` | b7579e27df2b | BSD-3-Clause | [LICENSE](https://github.com/golang/exp/blob/master/LICENSE) |
| `golang.org/x/sys` | v0.47.0 | BSD-3-Clause | [LICENSE](https://github.com/golang/sys/blob/master/LICENSE) |
| `modernc.org/libc` | v1.66.10 | BSD-3-Clause | [LICENSE](https://gitlab.com/cznic/libc/-/blob/master/LICENSE) |
| `modernc.org/mathutil` | v1.7.1 | BSD-3-Clause | [LICENSE](https://gitlab.com/cznic/mathutil/-/blob/master/LICENSE) |
| `modernc.org/memory` | v1.11.0 | BSD-3-Clause | [LICENSE](https://gitlab.com/cznic/memory/-/blob/master/LICENSE) |

The modernc.org modules form the SQLite implementation's transitive runtime
dependency set. The Go toolchain may also download additional modules in the
module graph; `go.mod` and `go.sum` are authoritative for a particular
checkout. The container image includes the applicable dependency license texts
under `/licenses`, including embedded subcomponent notices from the modernc.org
modules. Notices and source are also available from each upstream repository.

## Vendored frontend assets (shipped in the application image)

| Asset | Version | License | Upstream notice |
| --- | --- | --- | --- |
| `internal/web/assets/d3.min.js` (D3.js, self-hosted for the Activity graph) | v7.9.0 | ISC | `internal/web/assets/D3-LICENSE`, [upstream LICENSE](https://github.com/d3/d3/blob/main/LICENSE) |
| `internal/web/assets/media/providers/*.svg` (provider marks, except `llama-cpp.svg` and `generic-openai.svg`) | `@lobehub/icons-static-svg` v1.95.1 | MIT | [LobeHub lobe-icons LICENSE](https://github.com/lobehub/lobe-icons/blob/master/LICENSE) |
| `internal/web/assets/media/providers/llama-cpp.svg` (llama.cpp mark) | vendored from `ggml-org/llama.cpp` | MIT | [llama.cpp LICENSE](https://github.com/ggml-org/llama.cpp/blob/master/LICENSE) |
| `internal/web/assets/media/providers/generic-openai.svg` (generic OpenAI-compatible mark) | original project artwork | AGPL-3.0 | — (no third-party asset) |

Provider mark mapping (every catalogued provider type is covered): OpenAI → `openai.svg`; Codex Subscription → `codex.svg`; Anthropic → `anthropic.svg`; Claude Code Subscription → `claude-code.svg`; GitHub Copilot → `github-copilot.svg`; Google Gemini API → `gemini.svg`; DeepSeek → `deepseek.svg`; Z.ai / GLM → `zai.svg`; Azure OpenAI → `azure.svg`; Amazon Bedrock API key → `bedrock.svg`; Groq → `groq.svg`; Mistral → `mistral.svg`; xAI → `xai.svg`; Together → `together.svg`; Fireworks → `fireworks.svg`; Cerebras → `cerebras.svg`; Perplexity → `perplexity.svg`; Hugging Face → `huggingface.svg`; Cloudflare Workers AI → `cloudflare.svg`; Alibaba / Qwen → `qwen.svg`; MiniMax → `minimax.svg`; Command Code → `commandcode.svg`; Generic OpenAI-compatible → `generic-openai.svg`; vLLM → `vllm.svg`; LM Studio → `lm-studio.svg`; llama.cpp → `llama-cpp.svg`; OpenRouter → `openrouter.svg`; OpenCode Zen/Go/Free → `opencode.svg`; Ollama Local/Cloud → `ollama.svg`; NVIDIA NIM → `nvidia.svg`. The in-UI monogram is retained only as a fallback for unknown or future provider types. The LobeHub SVG paths are derived from the pinned `@lobehub/icons-static-svg` release and the llama.cpp mark is vendored verbatim from the upstream project; both are used under their respective MIT licenses. The generic OpenAI-compatible mark is original project artwork (AGPL-3.0), drawn in-house rather than sourced from a third party. Logos are decorative marks, not endorsements; provider names remain the identifying text.

## Test-only tooling and images

The browser test harness declares Playwright Test `1.55.0` in
`tests/browser/package.json` and `package-lock.json`. It is test-only and is
not shipped in the Tiller Router image; see the [Playwright license](https://github.com/microsoft/playwright/blob/main/LICENSE)
and the installed package notices when running the browser harness. The
compatibility and browser harnesses also use their declared test-container base
images, and the reverse-proxy smoke uses the official Nginx Alpine test image.
These test-only tools are not application runtime dependencies.

No provider SDK is included
in the application image. Review the upstream notices before redistributing a
modified build or a test environment.

## Optional Python gateway client

`clients/python` is distributed separately from the router image under AGPL-3.0.
Its direct runtime dependency is `httpx` (`>=0.28,<1`, BSD-3-Clause), which also
requires its own declared dependencies. The client does not import a provider
SDK. The Docker test build installs the client wheel and resolves those Python
dependencies; consuming projects should pin the resolved dependency set in
their own lock files. See [HTTPX's license](https://github.com/encode/httpx/blob/master/LICENSE.md).
