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

type Protocol string

const (
	ProtocolChat      Protocol = "chat"
	ProtocolResponses Protocol = "responses"
	ProtocolMessages  Protocol = "messages"
)

type Descriptor struct {
	Type             string     `json:"type"`
	Label            string     `json:"label"`
	DefaultBaseURL   string     `json:"default_base_url,omitempty"`
	BaseURLRequired  bool       `json:"base_url_required"`
	CredentialNeeded bool       `json:"credential_needed"`
	Protocols        []Protocol `json:"protocols"`
	Discovery        string     `json:"-"`
}

var descriptors = []Descriptor{
	{Type: "openai", Label: "OpenAI", DefaultBaseURL: "https://api.openai.com/v1", CredentialNeeded: true, Protocols: []Protocol{ProtocolChat, ProtocolResponses}, Discovery: "openai"},
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
	{Type: "opencode-free", Label: "OpenCode Free", DefaultBaseURL: "https://opencode.ai/zen/v1", Protocols: []Protocol{ProtocolChat}, Discovery: "opencode"},
	{Type: "generic-openai", Label: "Generic OpenAI-compatible", BaseURLRequired: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "vllm", Label: "vLLM", BaseURLRequired: true, Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "lm-studio", Label: "LM Studio", DefaultBaseURL: "http://host.docker.internal:1234/v1", Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
	{Type: "llama-cpp", Label: "llama.cpp", DefaultBaseURL: "http://host.docker.internal:8080/v1", Protocols: []Protocol{ProtocolChat}, Discovery: "openai"},
}

func Descriptors() []Descriptor { return append([]Descriptor(nil), descriptors...) }

func Lookup(providerType string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.Type == providerType {
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
	Enabled                             bool
	Protocols                           []Protocol
}

type Model struct {
	ID, DisplayName string
	ContextLength   int
	MaxOutputTokens int
	NativeProtocol  Protocol
	// Tri-state capability flags: nil = unknown, non-nil = supported/unsupported.
	SupportsTools, SupportsVision, SupportsReasoning, SupportsStructuredOutput *bool
	InputModalities, OutputModalities                                          []string
}

var openCodeZenProtocolByModel = map[string]Protocol{
	"gpt-5.6-sol": ProtocolResponses, "gpt-5.6-terra": ProtocolResponses, "gpt-5.6-luna": ProtocolResponses,
	"gpt-5.5": ProtocolResponses, "gpt-5.5-pro": ProtocolResponses, "gpt-5.4": ProtocolResponses,
	"gpt-5.4-pro": ProtocolResponses, "gpt-5.4-mini": ProtocolResponses, "gpt-5.4-nano": ProtocolResponses,
	"gpt-5.3-codex": ProtocolResponses, "gpt-5.3-codex-spark": ProtocolResponses, "gpt-5.2": ProtocolResponses,
	"gpt-5.2-codex": ProtocolResponses, "gpt-5.1": ProtocolResponses, "gpt-5.1-codex": ProtocolResponses,
	"gpt-5.1-codex-max": ProtocolResponses, "gpt-5.1-codex-mini": ProtocolResponses, "gpt-5": ProtocolResponses,
	"gpt-5-codex": ProtocolResponses, "gpt-5-nano": ProtocolResponses, "grok-4.6": ProtocolResponses,
	"grok-4.5": ProtocolResponses, "grok-build-0.1": ProtocolResponses, "muse-spark-1.2": ProtocolResponses,
	"claude-fable-5": ProtocolMessages, "claude-opus-5": ProtocolMessages, "claude-opus-4.8": ProtocolMessages,
	"claude-opus-4.7": ProtocolMessages, "claude-opus-4.6": ProtocolMessages, "claude-opus-4.5": ProtocolMessages,
	"claude-sonnet-5": ProtocolMessages, "claude-sonnet-4.6": ProtocolMessages, "claude-sonnet-4.5": ProtocolMessages,
	"claude-haiku-4.5": ProtocolMessages, "qwen3.7-max": ProtocolMessages, "qwen3.7-plus": ProtocolMessages,
	"qwen3.6-plus": ProtocolMessages, "qwen3.5-plus": ProtocolMessages,
}

func nativeProtocol(providerType, modelID string) Protocol {
	if (providerType == "opencode-zen" || providerType == "opencode-free") && strings.HasSuffix(modelID, "-free") {
		return ProtocolChat
	}
	if providerType == "opencode-zen" {
		if protocol, ok := openCodeZenProtocolByModel[modelID]; ok {
			return protocol
		}
		return ProtocolChat
	}
	if providerType == "opencode-go" {
		return ProtocolChat
	}
	return ""
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

func (r *Registry) Discover(ctx context.Context, provider Instance) ([]Model, error) {
	d, ok := Lookup(provider.Type)
	if !ok {
		return nil, fmt.Errorf("unsupported provider type %q", provider.Type)
	}
	var models []Model
	var err error
	switch d.Discovery {
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
			result = append(result, Model{ID: modelID, DisplayName: display, ContextLength: firstPositive(item.ContextLength, item.ContextWindow, item.MaxModelLen, item.MaxInputTokens), MaxOutputTokens: maxOutputTokens, NativeProtocol: nativeProtocol(provider.Type, modelID), SupportsTools: triBool(len(sp) > 0, slices.Contains(sp, "tools")), SupportsVision: triBool(len(arch.InputModalities) > 0, slices.Contains(arch.InputModalities, "image")), SupportsReasoning: triBool(len(sp) > 0, slices.Contains(sp, "reasoning")), SupportsStructuredOutput: triBool(len(sp) > 0, slices.Contains(sp, "structured_outputs")), InputModalities: arch.InputModalities, OutputModalities: arch.OutputModalities})
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
	if provider.Type == "anthropic" {
		req.Header.Set("x-api-key", provider.Credential)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else if provider.Type == "azure-openai" {
		req.Header.Set("api-key", provider.Credential)
	} else {
		req.Header.Set("Authorization", "Bearer "+provider.Credential)
	}
}

func Endpoint(provider Instance, protocol Protocol) (string, error) {
	var endpoint string
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
