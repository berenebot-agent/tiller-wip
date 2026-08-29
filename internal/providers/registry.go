package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
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

type Model struct{ ID, DisplayName string }

type Registry struct{ client *http.Client }

func NewRegistry() *Registry {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: time.Second, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second,
	}
	return &Registry{client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (r *Registry) HTTPClient() *http.Client { return r.client }

func (r *Registry) Discover(ctx context.Context, provider Instance) ([]Model, error) {
	d, ok := Lookup(provider.Type)
	if !ok {
		return nil, fmt.Errorf("unsupported provider type %q", provider.Type)
	}
	switch d.Discovery {
	case "ollama":
		return r.discoverOllama(ctx, provider)
	case "huggingface":
		return r.discoverHuggingFace(ctx, provider)
	case "cloudflare":
		return r.discoverCloudflare(ctx, provider)
	default:
		return r.discoverPaged(ctx, provider, d.Discovery == "anthropic")
	}
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
				ID, Name    string
				DisplayName string `json:"display_name"`
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
			result = append(result, Model{ID: modelID, DisplayName: display})
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
			out = append(out, Model{ID: modelID, DisplayName: modelID})
		}
	}
	return out, nil
}

func (r *Registry) discoverHuggingFace(ctx context.Context, provider Instance) ([]Model, error) {
	endpoint := "https://huggingface.co/api/models?inference=warm&limit=1000"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	ApplyRequestAuth(req, provider)
	var payload []struct {
		ID      string `json:"id"`
		ModelID string `json:"modelId"`
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
			out = append(out, Model{ID: modelID, DisplayName: modelID})
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
		Result []struct{ Name, ID string } `json:"result"`
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
			out = append(out, Model{ID: modelID, DisplayName: modelID})
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
	u.RawQuery = e.RawQuery
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
