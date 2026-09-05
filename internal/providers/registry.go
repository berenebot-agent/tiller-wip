package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Codex uses the Responses API over a subscription-backed OAuth credential.
const codexProviderType = "codex-subscription"

type Protocol string

type AuthMode string

const (
	ProtocolChat      Protocol = "chat"
	ProtocolResponses Protocol = "responses"
	ProtocolMessages  Protocol = "messages"
	AuthModeAPIKey    AuthMode = "api_key"
	AuthModeOAuth     AuthMode = "oauth"
)

type Descriptor struct {
	Type             string     `json:"type"`
	Label            string     `json:"label"`
	DefaultBaseURL   string     `json:"default_base_url,omitempty"`
	BaseURLRequired  bool       `json:"base_url_required"`
	CredentialNeeded bool       `json:"credential_needed"`
	AuthMode         AuthMode   `json:"auth_mode"`
	AuthFlow         string     `json:"auth_flow,omitempty"`
	Protocols        []Protocol `json:"protocols"`
	MinOutputTokens  int        `json:"min_output_tokens,omitempty"`
	Discovery        string     `json:"-"`
}

var descriptors = []Descriptor{
	{Type: "openai", Label: "OpenAI", DefaultBaseURL: "https://api.openai.com/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat, ProtocolResponses}, Discovery: "openai"},
	{Type: codexProviderType, Label: "Codex Subscription", DefaultBaseURL: "https://chatgpt.com/backend-api/codex", AuthMode: AuthModeOAuth, AuthFlow: "authorization_code_pkce", Protocols: []Protocol{ProtocolResponses}, Discovery: "codex"},
	{Type: "claude-subscription", Label: "Claude Code Subscription", DefaultBaseURL: "https://api.anthropic.com/v1", AuthMode: AuthModeOAuth, AuthFlow: "authorization_code_pkce", Protocols: []Protocol{ProtocolMessages}, Discovery: "claude"},
	{Type: "github-copilot", Label: "GitHub Copilot", DefaultBaseURL: "https://api.githubcopilot.com", AuthMode: AuthModeOAuth, AuthFlow: "device_code", Protocols: []Protocol{ProtocolChat, ProtocolResponses, ProtocolMessages}, Discovery: "github-copilot"},
	{Type: "anthropic", Label: "Anthropic", DefaultBaseURL: "https://api.anthropic.com/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolMessages}, Discovery: "anthropic"},
	{Type: "openrouter", Label: "OpenRouter", DefaultBaseURL: "https://openrouter.ai/api/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "ollama-local", Label: "Ollama Local", DefaultBaseURL: "http://host.docker.internal:11434", Protocols: []Protocol{ProtocolChat}, Discovery: "ollama"},
	{Type: "ollama-cloud", Label: "Ollama Cloud", DefaultBaseURL: "https://ollama.com", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "ollama"},
	{Type: "deepseek", Label: "DeepSeek", DefaultBaseURL: "https://api.deepseek.com", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "zai", Label: "Z.ai / GLM", DefaultBaseURL: "https://api.z.ai/api/paas/v4", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "gemini", Label: "Google Gemini API", DefaultBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "azure-openai", Label: "Azure OpenAI", BaseURLRequired: true, CredentialNeeded: true, Protocols: []Protocol{ProtocolChat, ProtocolResponses}, Discovery: "openai"},
	{Type: "bedrock-api-key", Label: "Amazon Bedrock API key", BaseURLRequired: true, CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "groq", Label: "Groq", DefaultBaseURL: "https://api.groq.com/openai/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "mistral", Label: "Mistral", DefaultBaseURL: "https://api.mistral.ai/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "xai", Label: "xAI", DefaultBaseURL: "https://api.x.ai/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "together", Label: "Together", DefaultBaseURL: "https://api.together.xyz/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "fireworks", Label: "Fireworks", DefaultBaseURL: "https://api.fireworks.ai/inference/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "cerebras", Label: "Cerebras", DefaultBaseURL: "https://api.cerebras.ai/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "perplexity", Label: "Perplexity", DefaultBaseURL: "https://api.perplexity.ai", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "nvidia-nim", Label: "NVIDIA NIM", DefaultBaseURL: "https://integrate.api.nvidia.com/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "huggingface", Label: "Hugging Face Inference", DefaultBaseURL: "https://router.huggingface.co/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "huggingface"},
	{Type: "cloudflare-ai", Label: "Cloudflare Workers AI", BaseURLRequired: true, CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "cloudflare"},
	{Type: "alibaba-qwen", Label: "Alibaba / Qwen", DefaultBaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "minimax", Label: "MiniMax", DefaultBaseURL: "https://api.minimax.io/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "opencode-zen", Label: "OpenCode Zen", DefaultBaseURL: "https://opencode.ai/zen/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat, ProtocolResponses, ProtocolMessages}, Discovery: "opencode"},
	{Type: "opencode-go", Label: "OpenCode Go", DefaultBaseURL: "https://opencode.ai/zen/go/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat, ProtocolResponses, ProtocolMessages}, Discovery: "opencode"},
	{Type: "opencode-free", Label: "OpenCode Free", DefaultBaseURL: "https://opencode.ai/zen/v1", Protocols: []Protocol{ProtocolChat, ProtocolResponses}, MinOutputTokens: 16, Discovery: "opencode"},
	{Type: "generic-openai", Label: "Generic OpenAI-compatible", BaseURLRequired: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "vllm", Label: "vLLM", BaseURLRequired: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "lm-studio", Label: "LM Studio", DefaultBaseURL: "http://host.docker.internal:1234/v1", Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "llama-cpp", Label: "llama.cpp", DefaultBaseURL: "http://host.docker.internal:8080/v1", Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
}

func Descriptors() []Descriptor {
	out := append([]Descriptor(nil), descriptors...)
	for i := range out {
		if out[i].AuthMode == "" {
			out[i].AuthMode = AuthModeAPIKey
		}
	}
	return out
}

func Lookup(providerType string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.Type == providerType {
			if descriptor.AuthMode == "" {
				descriptor.AuthMode = AuthModeAPIKey
			}
			return descriptor, true
		}
	}
	return Descriptor{}, false
}

func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("base_url must be an http(s) URL with a host and no userinfo or fragment")
	}
	return nil
}

type Instance struct {
	ID, Name, Type, BaseURL, Credential string
	OAuthAccountID                      string
	OAuthProviderData                   map[string]any
	OAuthState                          string
	Enabled                             bool
	Protocols                           []Protocol
	MinOutputTokens                     int
}

// ReasoningOptionType enumerates the selector mechanisms a model may expose.
type ReasoningOptionType string

const (
	ReasoningOptionEffort       ReasoningOptionType = "effort"
	ReasoningOptionToggle       ReasoningOptionType = "toggle"
	ReasoningOptionBudgetTokens ReasoningOptionType = "budget_tokens"
)

// ReasoningOption is a provider-neutral representation of a single reasoning
// selector mechanism. Values is populated for effort-type options. Min and Max
// (int64 pointers, never float) are populated for budget_tokens options.
type ReasoningOption struct {
	Type   ReasoningOptionType `json:"type"`
	Values []string            `json:"values,omitempty"`
	Min    *int64              `json:"min,omitempty"`
	Max    *int64              `json:"max,omitempty"`
}

// ReasoningCapabilities is the normalized, provider-neutral reasoning metadata
// for a model. nil means option metadata is unknown; a non-nil struct with an
// empty Options list means the source explicitly reported no configurable
// selector.
type ReasoningCapabilities struct {
	Options        []ReasoningOption `json:"options"`
	ThinkingModes  []string          `json:"thinking_modes,omitempty"`
	DefaultEffort  string            `json:"default_effort,omitempty"`
	Mandatory      *bool             `json:"mandatory,omitempty"`
	DefaultEnabled *bool             `json:"default_enabled,omitempty"`
	Parameters     []string          `json:"parameters,omitempty"`
}

// ReasoningOptions is the set of selector mechanisms a model supports, derived
// from Options. It is used by the mapper to decide what a target accepts.
type ReasoningOptions struct {
	SupportsEffort   bool
	SupportedEfforts []string
	SupportsDisable  bool
	SupportsBudget   bool
	BudgetMin        *int64
	BudgetMax        *int64
	SupportsToggle   bool
	SupportsAdaptive bool
	SupportsEnabled  bool
}

// ExtractReasoningOptions derives the set of supported reasoning mechanisms
// from a capabilities struct for use by the mapper.
func ExtractReasoningOptions(caps *ReasoningCapabilities) ReasoningOptions {
	var r ReasoningOptions
	if caps == nil {
		return r
	}
	for _, opt := range caps.Options {
		switch opt.Type {
		case ReasoningOptionEffort:
			r.SupportsEffort = true
			r.SupportedEfforts = opt.Values
			for _, v := range opt.Values {
				if v == "none" {
					r.SupportsDisable = true
				}
			}
		case ReasoningOptionBudgetTokens:
			r.SupportsBudget = true
			if opt.Min != nil && (r.BudgetMin == nil || *opt.Min < *r.BudgetMin) {
				v := *opt.Min
				r.BudgetMin = &v
			}
			if opt.Max != nil && (r.BudgetMax == nil || *opt.Max > *r.BudgetMax) {
				v := *opt.Max
				r.BudgetMax = &v
			}
		case ReasoningOptionToggle:
			r.SupportsToggle = true
		}
	}
	for _, mode := range caps.ThinkingModes {
		switch mode {
		case "adaptive":
			r.SupportsAdaptive = true
		case "enabled":
			r.SupportsEnabled = true
		}
	}
	return r
}

// Canonical effort ordering for de-duplication and emission. Unknown
// provider-specific values are appended after these in encounter order.
var canonicalEffortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// CanonicalEffortOrder returns the canonical ordering of effort values.
func CanonicalEffortOrder() []string {
	return canonicalEffortOrder
}

func effortIndex(value string) int {
	for i, known := range canonicalEffortOrder {
		if known == value {
			return i
		}
	}
	return -1
}

// SortEfforts de-duplicates and orders effort values: known values first in
// canonical order, then unknown values in encounter order.
func SortEfforts(values []string) []string {
	seen := make(map[string]bool, len(values))
	var known, unknown []string
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		if effortIndex(v) >= 0 {
			known = append(known, v)
		} else {
			unknown = append(unknown, v)
		}
	}
	sort.Slice(known, func(i, j int) bool { return effortIndex(known[i]) < effortIndex(known[j]) })
	return append(known, unknown...)
}

type Model struct {
	ID, DisplayName string
	ContextLength   int
	MaxOutputTokens int
	NativeProtocol  Protocol
	// Tri-state capability flags: nil = unknown, non-nil = supported/unsupported.
	SupportsTools, SupportsVision, SupportsReasoning, SupportsStructuredOutput *bool
	// ReasoningCapabilities holds normalized selector metadata. nil means
	// unknown; a non-nil struct describes the advertised selectors.
	ReasoningCapabilities             *ReasoningCapabilities
	InputModalities, OutputModalities []string
}

var openCodeZenProtocolByModel = map[string]Protocol{
	"gpt-5.6-sol": ProtocolResponses, "gpt-5.6-terra": ProtocolResponses, "gpt-5.6-luna": ProtocolResponses,
	"gpt-5.5": ProtocolResponses, "gpt-5.5-pro": ProtocolResponses, "gpt-5.4": ProtocolResponses,
	"gpt-5.4-pro": ProtocolResponses, "gpt-5.4-mini": ProtocolResponses, "gpt-5.4-nano": ProtocolResponses,
	"gpt-5.3-codex": ProtocolResponses, "gpt-5.3-codex-spark": ProtocolResponses, "gpt-5.2": ProtocolResponses,
	"gpt-5.2-codex": ProtocolResponses, "gpt-5.1": ProtocolResponses, "gpt-5.1-codex": ProtocolResponses,
	"gpt-5.1-codex-max": ProtocolResponses, "gpt-5.1-codex-mini": ProtocolResponses, "gpt-5": ProtocolResponses,
	"gpt-5-codex": ProtocolResponses, "gpt-5-nano": ProtocolResponses, "grok-4.6": ProtocolResponses,
	"grok-4.5": ProtocolResponses, "grok-build-0.1": ProtocolResponses, "muse-spark-1.2": ProtocolResponses, "muse-spark-1.2-contributor-free": ProtocolResponses, "muse-spark-1.3-contributor-free": ProtocolResponses,
	"claude-fable-5": ProtocolMessages, "claude-opus-5": ProtocolMessages, "claude-opus-4.8": ProtocolMessages,
	"claude-opus-4.7": ProtocolMessages, "claude-opus-4.6": ProtocolMessages, "claude-opus-4.5": ProtocolMessages,
	"claude-sonnet-5": ProtocolMessages, "claude-sonnet-4.6": ProtocolMessages, "claude-sonnet-4.5": ProtocolMessages,
	"claude-haiku-4.5": ProtocolMessages, "qwen3.7-max": ProtocolMessages, "qwen3.7-plus": ProtocolMessages,
	"qwen3.6-plus": ProtocolMessages, "qwen3.5-plus": ProtocolMessages,
}

func nativeProtocol(providerType, modelID string) Protocol {
	if providerType == "opencode-zen" || providerType == "opencode-free" {
		if protocol, ok := openCodeZenProtocolByModel[modelID]; ok {
			return protocol
		}
		if openCodeResponsesModel(modelID) {
			return ProtocolResponses
		}
		return ProtocolChat
	}
	if providerType == "opencode-go" {
		return ProtocolChat
	}
	return ""
}

func openCodeResponsesModel(modelID string) bool {
	lowerModelID := strings.ToLower(modelID)
	if strings.Contains(lowerModelID, "response") {
		return true
	}
	return strings.HasPrefix(lowerModelID, "gpt-5") ||
		strings.HasPrefix(lowerModelID, "grok-4") ||
		strings.HasPrefix(lowerModelID, "grok-build-") ||
		strings.HasPrefix(lowerModelID, "muse-spark-")
}

type Registry struct {
	client           *http.Client
	mu               sync.Mutex
	modelsDev        modelsDevDataset
	modelsDevEnabled bool
}

func NewRegistry() *Registry {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		// Keep a stalled ordered-fallback target from consuming the client's
		// entire request deadline. Once headers arrive, streaming may continue
		// without this header timeout interrupting the response body.
		// 60s: large-prompt requests to cloud LLMs (e.g. ollama) can legitimately
		// take longer than 15s to return response headers; 15s caused spurious
		// upstream_timeout -> virtual_model_unavailable on big cron prompts.
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second,
	}
	return &Registry{client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, modelsDevEnabled: true}
}

// SetResponseHeaderTimeout updates the per-attempt time-to-first-header bound on
// the shared HTTP transport. It is what keeps a stalled ordered-fallback target
// from consuming the client's request deadline; once headers arrive, streaming
// continues unbounded. Guarded by a mutex because the transport is shared.
func (r *Registry) SetResponseHeaderTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.client.Transport.(*http.Transport); ok {
		t.ResponseHeaderTimeout = d
	}
}

func (r *Registry) HTTPClient() *http.Client { return r.client }

// SetHTTPClient replaces the registry's HTTP client. Intended for tests that
// need to route OAuth and upstream requests to mock servers.
func (r *Registry) SetHTTPClient(client *http.Client) { r.client = client }

func (r *Registry) Discover(ctx context.Context, provider Instance) ([]Model, error) {
	d, ok := Lookup(provider.Type)
	if !ok {
		return nil, fmt.Errorf("unsupported provider type %q", provider.Type)
	}
	var models []Model
	var err error
	switch d.Discovery {
	case "codex":
		models = codexModels()
	case "claude":
		models = claudeModels()
	case "github-copilot":
		models = githubCopilotModels()
	case "ollama":
		models, err = r.discoverOllama(ctx, provider)
	case "huggingface":
		models, err = r.discoverHuggingFace(ctx, provider)
	case "cloudflare":
		models, err = r.discoverCloudflare(ctx, provider)
	default:
		models, err = r.discoverPaged(ctx, provider, d.Discovery == "anthropic")
	}
	if err != nil {
		return nil, err
	}
	// opencode-free is the anonymous, keyless tier — only models whose ID
	// ends with -free are usable without a credential. The upstream catalogue
	// at /zen/v1/models lists all 64 Zen models, so we filter here to surface
	// only the 7 free ones. The suffix convention is used by the provider and
	// is stable for future free models.
	if provider.Type == "opencode-free" {
		filtered := models[:0]
		for _, m := range models {
			if strings.HasSuffix(m.ID, "-free") {
				filtered = append(filtered, m)
			}
		}
		models = filtered
	}
	// Merge models.dev capability metadata as a fallback so real models whose
	// provider does not report capabilities still surface useful metadata. The
	// merged slice flows into Manager.applyCatalogue and is stored in the DB.
	return r.enrich(models, provider.Type), nil
}

func codexModels() []Model {
	ids := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark"}
	models := make([]Model, 0, len(ids))
	for _, id := range ids {
		models = append(models, Model{ID: id, DisplayName: id, NativeProtocol: ProtocolResponses})
	}
	return models
}

func claudeModels() []Model {
	ids := []string{"claude-opus-5", "claude-fable-5-1", "claude-fable-5", "claude-sonnet-5", "claude-haiku-4-5-20251001"}
	models := make([]Model, 0, len(ids))
	for _, modelID := range ids {
		models = append(models, Model{ID: modelID, DisplayName: modelID, NativeProtocol: ProtocolMessages})
	}
	return models
}

func githubCopilotModels() []Model {
	entries := []struct {
		id       string
		protocol Protocol
	}{
		// Codex variants route through Responses (matching the official Codex
		// client); non-Codex GPT models stay on Chat Completions.
		{"gpt-5.2", ProtocolChat}, {"gpt-5.2-codex", ProtocolResponses}, {"gpt-5.3-codex", ProtocolResponses}, {"gpt-5.4", ProtocolChat}, {"gpt-5.4-mini", ProtocolChat},
		{"claude-haiku-4.5", ProtocolMessages}, {"claude-opus-4.5", ProtocolMessages}, {"claude-sonnet-4.5", ProtocolMessages}, {"claude-sonnet-4.6", ProtocolMessages}, {"claude-opus-4.6", ProtocolMessages}, {"claude-opus-4.7", ProtocolMessages},
		{"gemini-2.5-pro", ProtocolChat}, {"gemini-3-flash-preview", ProtocolChat}, {"gemini-3.1-pro-preview", ProtocolChat}, {"grok-code-fast-1", ProtocolChat},
	}
	models := make([]Model, 0, len(entries))
	for _, entry := range entries {
		models = append(models, Model{ID: entry.id, DisplayName: entry.id, NativeProtocol: entry.protocol})
	}
	return models
}

// parseReasoningCapabilities selects the correct parser for a provider type
// and returns the normalized capabilities. Returns nil when no reasoning
// metadata is reported by the provider. supportedParams is the top-level
// supported_parameters array from the model entry (used as fallback for
// parameter hints when the reasoning object omits them).
func parseReasoningCapabilities(providerType string, reasoningObj, capabilitiesObj any, supportedParams []string) *ReasoningCapabilities {
	if providerType == "anthropic" {
		return anthropicReasoning(capabilitiesObj)
	}
	if reasoningObj != nil {
		if rc := openRouterReasoning(reasoningObj, supportedParams); rc != nil {
			return rc
		}
	}
	return nil
}

// openRouterReasoning parses an OpenRouter-style `reasoning` object from a
// model entry. Returns nil when the field is absent or not an object.
//
// Real OpenRouter API shape (from GET /api/v1/models):
//
//	{
//	  "id": "openai/gpt-5",
//	  "supported_parameters": ["tools", "reasoning", "reasoning_effort", ...],
//	  "reasoning": {
//	    "supported_efforts": ["low", "medium", "high"],
//	    "default_effort": "medium",
//	    "mandatory": false,
//	    "default_enabled": true,
//	    "supports_max_tokens": true
//	  }
//	}
//
// Key edge cases:
//   - supported_efforts omitted (field absent): no effort selector exposed.
//   - supported_efforts: null: all gateway effort values accepted (effort selector
//     present but unrestricted).
//   - supported_efforts: []: present but empty — treated as no effort selector.
//   - mandatory: true + effort="none": none is not a valid user choice; the gateway
//     will reject it. The mapper must not send none when mandatory is set.
func openRouterReasoning(raw any, topLevelParams []string) *ReasoningCapabilities {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	var rc ReasoningCapabilities

	// supported_efforts: distinguish absent vs null vs list.
	if v, exists := obj["supported_efforts"]; exists {
		if v == nil {
			// null — all gateway effort values accepted. Mark effort as
			// supported with empty values (mapper will pass through any effort).
			rc.Options = append(rc.Options, ReasoningOption{Type: ReasoningOptionEffort})
		} else if arr, ok := v.([]any); ok && len(arr) > 0 {
			var efforts []string
			for _, e := range arr {
				if s, ok := e.(string); ok {
					efforts = append(efforts, s)
				}
			}
			if len(efforts) > 0 {
				rc.Options = append(rc.Options, ReasoningOption{Type: ReasoningOptionEffort, Values: SortEfforts(efforts)})
			}
		}
	}

	if v, ok := obj["default_effort"].(string); ok && v != "" {
		rc.DefaultEffort = v
	}
	if v, ok := obj["mandatory"].(bool); ok {
		rc.Mandatory = &v
	}
	if v, ok := obj["default_enabled"].(bool); ok {
		rc.DefaultEnabled = &v
	}
	if v, ok := obj["supports_max_tokens"].(bool); ok && v {
		rc.Options = append(rc.Options, ReasoningOption{Type: ReasoningOptionBudgetTokens})
	}
	if v, ok := obj["supported_parameters"].([]any); ok {
		for _, p := range v {
			if s, ok := p.(string); ok && (s == "reasoning" || s == "reasoning_effort" || s == "include_reasoning") {
				rc.Parameters = append(rc.Parameters, s)
			}
		}
	}
	// Fall back to top-level supported_parameters if the reasoning object
	// carried none.
	if len(rc.Parameters) == 0 {
		for _, p := range topLevelParams {
			if p == "reasoning" || p == "reasoning_effort" || p == "include_reasoning" {
				rc.Parameters = append(rc.Parameters, p)
			}
		}
	}
	if len(rc.Options) == 0 && rc.DefaultEffort == "" && rc.Mandatory == nil && rc.DefaultEnabled == nil && len(rc.Parameters) == 0 {
		return nil
	}
	return &rc
}

// anthropicReasoning parses an Anthropic-style model entry with
// capabilities.effort and capabilities.thinking levels. Returns nil when no
// reasoning capability data is present.
//
// Anthropic's current Models API distinguishes thinking.types.adaptive from
// thinking.types.enabled (legacy). We model them separately so the mapper can
// avoid silently dropping adaptive thinking during cross-protocol translation.
func anthropicReasoning(capabilitiesRaw any) *ReasoningCapabilities {
	caps, ok := capabilitiesRaw.(map[string]any)
	if !ok {
		return nil
	}
	var rc ReasoningCapabilities
	if effortRaw, ok := caps["effort"].(map[string]any); ok {
		var efforts []string
		for _, level := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
			if lvl, ok := effortRaw[level].(map[string]any); ok {
				if supported, ok := lvl["supported"].(bool); ok && supported {
					efforts = append(efforts, level)
				}
			}
		}
		if len(efforts) > 0 {
			rc.Options = append(rc.Options, ReasoningOption{Type: ReasoningOptionEffort, Values: SortEfforts(efforts)})
		}
	}
	if thinkingRaw, ok := caps["thinking"].(map[string]any); ok {
		if typesRaw, ok := thinkingRaw["types"].(map[string]any); ok {
			for _, mode := range []string{"adaptive", "enabled"} {
				if modeRaw, ok := typesRaw[mode].(map[string]any); ok {
					if supported, ok := modeRaw["supported"].(bool); ok && supported {
						rc.ThinkingModes = append(rc.ThinkingModes, mode)
					}
				}
			}
		} else if supported, ok := thinkingRaw["supported"].(bool); ok && supported {
			// Legacy: no types breakdown — treat as generic toggle.
			rc.Options = append(rc.Options, ReasoningOption{Type: ReasoningOptionToggle})
		}
	}
	if len(rc.Options) == 0 && len(rc.ThinkingModes) == 0 {
		return nil
	}
	return &rc
}

func (r *Registry) discoverPaged(ctx context.Context, provider Instance, anthropic bool) ([]Model, error) {
	endpoint, err := appendEndpoint(provider.BaseURL, "models")
	if err != nil {
		return nil, err
	}
	var result []Model
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		ApplyRequestAuth(req, provider)
		if anthropic {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("model discovery returned HTTP %d", resp.StatusCode)
		}
		var payload struct {
			Data []struct {
				ID, Name        string
				DisplayName     string `json:"display_name"`
				ContextLength   int    `json:"context_length"`
				ContextWindow   int    `json:"context_window"`
				MaxModelLen     int    `json:"max_model_len"`
				MaxInputTokens  int    `json:"max_input_tokens"`
				MaxOutputTokens int    `json:"max_output_tokens"`
				TopProvider     struct {
					MaxCompletionTokens int `json:"max_completion_tokens"`
				} `json:"top_provider"`
				SupportedParameters []string `json:"supported_parameters"`
				Architecture        struct {
					InputModalities  []string `json:"input_modalities"`
					OutputModalities []string `json:"output_modalities"`
				} `json:"architecture"`
				// OpenRouter exposes reasoning metadata as a nested object.
				Reasoning    map[string]any `json:"reasoning"`
				Capabilities map[string]any `json:"capabilities"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
			Next    string `json:"next"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode model catalogue: %w", err)
		}
		for _, item := range payload.Data {
			modelID := item.ID
			if modelID == "" {
				modelID = item.Name
			}
			if modelID == "" || seen[modelID] {
				continue
			}
			seen[modelID] = true
			display := item.DisplayName
			if display == "" {
				display = item.Name
			}
			maxOutputTokens := item.MaxOutputTokens
			if provider.Type == "openrouter" {
				maxOutputTokens = firstPositive(maxOutputTokens, item.TopProvider.MaxCompletionTokens)
			}
			sp := item.SupportedParameters
			arch := item.Architecture
			reasoningCaps := parseReasoningCapabilities(provider.Type, item.Reasoning, item.Capabilities, sp)
			result = append(result, Model{
				ID: modelID, DisplayName: display,
				ContextLength:            firstPositive(item.ContextLength, item.ContextWindow, item.MaxModelLen, item.MaxInputTokens),
				MaxOutputTokens:          maxOutputTokens,
				NativeProtocol:           nativeProtocol(provider.Type, modelID),
				SupportsTools:            triBool(len(sp) > 0, slices.Contains(sp, "tools")),
				SupportsVision:           triBool(len(arch.InputModalities) > 0, slices.Contains(arch.InputModalities, "image")),
				SupportsReasoning:        triBool(len(sp) > 0, slices.Contains(sp, "reasoning")),
				SupportsStructuredOutput: triBool(len(sp) > 0, slices.Contains(sp, "structured_outputs")),
				ReasoningCapabilities:    reasoningCaps,
				InputModalities:          arch.InputModalities,
				OutputModalities:         arch.OutputModalities,
			})
		}
		if !payload.HasMore && payload.Next == "" {
			break
		}
		next := payload.Next
		if next == "" && payload.LastID != "" {
			u, _ := url.Parse(endpoint)
			q := u.Query()
			q.Set("after", payload.LastID)
			u.RawQuery = q.Encode()
			next = u.String()
		}
		if next == "" {
			return nil, errors.New("catalogue indicated another page without a cursor")
		}
		if err := sameOrigin(provider.BaseURL, next); err != nil {
			return nil, err
		}
		endpoint = next
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (r *Registry) discoverOllama(ctx context.Context, provider Instance) ([]Model, error) {
	endpoint, err := appendEndpoint(provider.BaseURL, "api/tags")
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	ApplyRequestAuth(req, provider)
	var payload struct {
		Models []struct{ Name, Model string } `json:"models"`
	}
	if err := r.doJSON(req, &payload); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(payload.Models))
	for _, item := range payload.Models {
		modelID := item.Name
		if modelID == "" {
			modelID = item.Model
		}
		if modelID != "" {
			out = append(out, Model{ID: modelID, DisplayName: modelID, ContextLength: r.ollamaContextLength(ctx, provider, modelID)})
		}
	}
	return out, nil
}

func (r *Registry) ollamaContextLength(ctx context.Context, provider Instance, modelID string) int {
	showEndpoint, err := appendEndpoint(provider.BaseURL, "api/show")
	if err != nil {
		return 0
	}
	body, _ := json.Marshal(map[string]string{"model": modelID})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, showEndpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ApplyRequestAuth(req, provider)
	var payload struct {
		ModelInfo  map[string]any `json:"model_info"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := r.doJSON(req, &payload); err != nil {
		return 0
	}
	// llama.context_length is the model's trained context window.
	if n, ok := coerceInt(payload.ModelInfo["llama.context_length"]); ok {
		return n
	}
	// Ollama prefixes architecture-specific metadata keys. DeepSeek and newer
	// architectures therefore expose e.g. deepseek.context_length rather than
	// llama.context_length. Accept any architecture context key after the
	// canonical llama key has been checked.
	keys := make([]string, 0, len(payload.ModelInfo))
	for key := range payload.ModelInfo {
		if strings.HasSuffix(key, ".context_length") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if n, ok := coerceInt(payload.ModelInfo[key]); ok {
			return n
		}
	}
	// Fall back to the runtime num_ctx from the Modelfile.
	if n, ok := coerceInt(payload.Parameters["num_ctx"]); ok {
		return n
	}
	return 0
}

func (r *Registry) discoverHuggingFace(ctx context.Context, provider Instance) ([]Model, error) {
	endpoint := "https://huggingface.co/api/models?inference=warm&limit=1000"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	ApplyRequestAuth(req, provider)
	var payload []struct {
		ID      string `json:"id"`
		ModelID string `json:"modelId"`
		Config  struct {
			MaxPositionEmbeddings int `json:"max_position_embeddings"`
		} `json:"config"`
	}
	if err := r.doJSON(req, &payload); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(payload))
	for _, item := range payload {
		modelID := item.ID
		if modelID == "" {
			modelID = item.ModelID
		}
		if modelID != "" {
			out = append(out, Model{ID: modelID, DisplayName: modelID, ContextLength: item.Config.MaxPositionEmbeddings})
		}
	}
	return out, nil
}

func (r *Registry) discoverCloudflare(ctx context.Context, provider Instance) ([]Model, error) {
	endpoint, err := appendEndpoint(provider.BaseURL, "models/search")
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	ApplyRequestAuth(req, provider)
	var payload struct {
		Result []struct {
			Name, ID       string
			ContextLength  int `json:"context_length"`
			MaxInputTokens int `json:"max_input_tokens"`
			MaxModelLen    int `json:"max_model_len"`
		} `json:"result"`
	}
	if err := r.doJSON(req, &payload); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(payload.Result))
	for _, item := range payload.Result {
		modelID := item.Name
		if modelID == "" {
			modelID = item.ID
		}
		if modelID != "" {
			out = append(out, Model{ID: modelID, DisplayName: modelID, ContextLength: firstPositive(item.ContextLength, item.MaxInputTokens, item.MaxModelLen)})
		}
	}
	return out, nil
}

func (r *Registry) doJSON(req *http.Request, target any) error {
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("model discovery returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(target); err != nil {
		return fmt.Errorf("decode model catalogue: %w", err)
	}
	return nil
}

func ApplyRequestAuth(req *http.Request, provider Instance) {
	req.Header.Set("Accept", "application/json")
	if provider.Credential == "" {
		return
	}
	if provider.Type == "claude-subscription" {
		req.Header.Set("Authorization", "Bearer "+provider.Credential)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14")
		req.Header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
		req.Header.Set("User-Agent", "claude-cli/2.1.251 (external, sdk-cli)")
		req.Header.Set("X-App", "cli")
	} else if provider.Type == "github-copilot" {
		token := provider.Credential
		if value, ok := provider.OAuthProviderData["copilot_token"].(string); ok && value != "" {
			token = value
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("copilot-integration-id", "vscode-chat")
		req.Header.Set("editor-version", "vscode/1.85.0")
		req.Header.Set("editor-plugin-version", "copilot-chat/0.26.7")
		req.Header.Set("user-agent", "GitHubCopilotChat/0.26.7")
		req.Header.Set("openai-intent", "conversation-panel")
		req.Header.Set("x-github-api-version", "2025-04-01")
		req.Header.Set("X-Initiator", "user")
	} else if provider.Type == "anthropic" {
		req.Header.Set("x-api-key", provider.Credential)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else if provider.Type == "azure-openai" {
		req.Header.Set("api-key", provider.Credential)
	} else {
		req.Header.Set("Authorization", "Bearer "+provider.Credential)
	}
	if provider.Type == codexProviderType {
		req.Header.Set("originator", "codex_cli_rs")
		req.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
		if provider.OAuthAccountID != "" {
			req.Header.Set("ChatGPT-Account-ID", provider.OAuthAccountID)
		}
	}
}

func Endpoint(provider Instance, protocol Protocol) (string, error) {
	var endpoint string
	if provider.Type == "github-copilot" {
		switch protocol {
		case ProtocolMessages:
			endpoint = "v1/messages"
		case ProtocolResponses:
			endpoint = "responses"
		default:
			endpoint = "chat/completions"
		}
		return appendEndpoint(provider.BaseURL, endpoint)
	}
	switch protocol {
	case ProtocolChat:
		endpoint = "chat/completions"
		if strings.HasPrefix(provider.Type, "ollama-") && !strings.HasSuffix(strings.TrimRight(provider.BaseURL, "/"), "/v1") {
			endpoint = "v1/chat/completions"
		}
	case ProtocolResponses:
		endpoint = "responses"
	case ProtocolMessages:
		endpoint = "v1/messages"
	default:
		return "", errors.New("unknown protocol")
	}
	return appendEndpoint(provider.BaseURL, endpoint)
}

func appendEndpoint(baseURL, endpoint string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	e, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimSuffix(u.Path, "/")
	if strings.HasPrefix(endpoint, "v1/") && strings.HasSuffix(basePath, "/v1") {
		e.Path = strings.TrimPrefix(strings.TrimPrefix(e.Path, "/"), "v1/")
	}
	u.Path = path.Join(basePath+"/", e.Path)
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	query := u.Query()
	for key, values := range e.Query() {
		// Endpoint-specific parameters are authoritative for duplicate keys,
		// while preserving repeated values and all base-only parameters.
		query.Del(key)
		for _, value := range values {
			query.Add(key, value)
		}
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func sameOrigin(baseURL, next string) error {
	base, err := url.Parse(baseURL)
	if err != nil {
		return err
	}
	n, err := url.Parse(next)
	if err != nil {
		return err
	}
	if !n.IsAbs() {
		return nil
	}
	if !strings.EqualFold(base.Scheme, n.Scheme) || !strings.EqualFold(base.Host, n.Host) {
		return errors.New("catalogue pagination attempted a cross-origin redirect")
	}
	return nil
}

func EncodeProtocols(protocols []Protocol) string {
	body, _ := json.Marshal(protocols)
	return string(body)
}
func DecodeProtocols(raw string) []Protocol {
	var p []Protocol
	if json.Unmarshal([]byte(raw), &p) != nil || len(p) == 0 {
		return []Protocol{ProtocolChat}
	}
	return p
}
func Supports(protocols []Protocol, protocol Protocol) bool {
	for _, p := range protocols {
		if p == protocol {
			return true
		}
	}
	return false
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

// triBool returns a tri-state capability value: nil when the provider did not
// report the field (unknown), otherwise a pointer to the reported boolean.
func triBool(present, val bool) *bool {
	if !present {
		return nil
	}
	b := val
	return &b
}

func coerceInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n > 0 && n == float64(int(n)) {
			return int(n), true
		}
	case int:
		if n > 0 {
			return n, true
		}
	case string:
		var parsed int
		if _, err := fmt.Sscanf(n, "%d", &parsed); err == nil && parsed > 0 {
			return parsed, true
		}
	}
	return 0, false
}
