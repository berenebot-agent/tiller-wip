package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// models.dev is an open-source, community-maintained registry of model metadata
// (context length, max output, capability flags, modalities). Tiller uses it as
// a *fallback* only: provider-reported data stays the source of truth. For
// most fields, models.dev fills in only the fields the provider left unknown; a
// field stays unknown if neither source reports it (tri-state semantics
// preserved). ReasoningCapabilities is handled differently: a provider-reported
// object is kept whole, and models.dev only fills individual mechanisms/fields
// the provider omitted (an explicitly empty provider object is left intact).

const (
	// modelsDevCacheFile is the cache file name under the data directory.
	modelsDevCacheFile = "models-dev.json"
	// modelsDevRefreshInterval is how often the background refresh runs.
	modelsDevRefreshInterval = 24 * time.Hour
	// modelsDevMaxAge is how old a cache copy may be before it is refreshed.
	modelsDevMaxAge = 24 * time.Hour
)

// modelsDevURL is the models.dev dataset endpoint (~4.4 MB JSON keyed by
// provider id, each with a "models" object keyed by model id). It is a var so
// tests can point it at a local server.
var modelsDevURL = "https://models.dev/api.json"

// ModelsDevCacheFile returns the cache file name (relative to the data
// directory) where the models.dev dataset is stored.
func ModelsDevCacheFile() string { return modelsDevCacheFile }

// modelsDevDataset is the parsed models.dev dataset keyed by provider id.
type modelsDevDataset map[string]modelsDevProvider

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	Reasoning        *bool               `json:"reasoning"`
	ToolCall         *bool               `json:"tool_call"`
	StructuredOutput *bool               `json:"structured_output"`
	Modalities       modelsDevModalities `json:"modalities"`
	Limit            modelsDevLimit      `json:"limit"`
	// reasoning_options is the raw options array from models.dev. Each entry
	// is parsed individually; a malformed entry is skipped while valid siblings
	// are retained.
	ReasoningOptions *[]map[string]any `json:"reasoning_options"`
}

type modelsDevModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type modelsDevLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// modelsDevProviderKey maps a tiller-router provider.Type to the models.dev
// provider key. Providers not listed here are never enriched (their models.dev
// key is unknown or ambiguous).
var modelsDevProviderKey = map[string]string{
	"openrouter":         "openrouter",
	"deepseek":           "deepseek",
	"nvidia-nim":         "nvidia",
	"zai":                "zhipuai",
	"gemini":             "google",
	"alibaba-qwen":       "alibaba",
	"fireworks":          "fireworks-ai",
	"azure-openai":       "azure",
	"opencode-zen":       "opencode",
	"opencode-go":        "opencode",
	"opencode-free":      "opencode",
	"openai":             "openai",
	"anthropic":          "anthropic",
	"groq":               "groq",
	"mistral":            "mistral",
	"xai":                "xai",
	"cerebras":           "cerebras",
	"perplexity":         "perplexity",
	"minimax":            "minimax",
	"huggingface":        "huggingface",
	"codex-subscription": "openai",
}

// Gateway providers may expose a model through a compatibility layer whose
// selector support does not necessarily match the upstream model metadata.
var gatewayProviderTypes = map[string]bool{
	"openrouter": true, "opencode-zen": true, "opencode-go": true,
	"opencode-free": true, "azure-openai": true, "groq": true,
	"fireworks": true, "nvidia-nim": true, "huggingface": true,
}

// ollamaLabInference maps a recognizable model-family root (the lowercased,
// tag/namespace-stripped model name) to the canonical models.dev lab that holds
// exact, plain-key entries for that family. It is deliberately small: only
// families with a verified canonical lab that stores the model under its plain
// name are listed, so the lookup is deterministic and never relies on fuzzy
// basename matching across labs (which diverges for many models). Families
// without such a canonical lab (e.g. qwen/llama/gemma/phi) are intentionally
// omitted rather than guessed.
var ollamaLabInference = []struct{ root, lab string }{
	{"glm", "zai"},
	{"deepseek", "deepseek"},
	{"mistral", "mistral"},
}

// ollamaLookup resolves a models.dev entry for an Ollama model. Ollama model
// IDs carry an optional `:tag` suffix and an optional `namespace/` prefix, and
// models.dev has no `ollama` lab — open-weights models live under their origin
// lab's plain name. The plain name is matched against the inferred canonical
// lab only.
func ollamaLookup(data modelsDevDataset, id string) modelsDevModel {
	plain := ollamaPlainName(id)
	var lab string
	ok := false
	l := strings.ToLower(plain)
	for _, e := range ollamaLabInference {
		if strings.HasPrefix(l, e.root) {
			lab, ok = e.lab, true
			break
		}
	}
	if !ok {
		return modelsDevModel{}
	}
	provider, ok := data[lab]
	if !ok {
		return modelsDevModel{}
	}
	return provider.Models[plain]
}

// ollamaPlainName strips the trailing `:tag` (e.g. `glm-5.3-flash:latest`) and
// any leading `namespace/` prefix (e.g. `zai/glm-5.3-flash`) to recover the
// plain model name models.dev keys models by.
func ollamaPlainName(id string) string {
	if i := strings.LastIndex(id, ":"); i >= 0 {
		id = id[:i]
	}
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// enrich merges models.dev capability metadata into the discovered models for a
// provider. For most fields it fills in only the fields the provider left
// unknown and never overrides a provider-reported value. ReasoningCapabilities
// uses whole-object provider precedence with field-level gap fill. It is a
// no-op when models.dev is disabled
// or the dataset is unavailable. Non-Ollama providers are looked up by their
// exact provider key + model ID; Ollama has no lab of its own, so its models are
// resolved by plain (tag/namespace-stripped) name under an inferred canonical
// lab.
func (r *Registry) enrich(models []Model, providerType string) []Model {
	r.mu.Lock()
	enabled := r.modelsDevEnabled
	data := r.modelsDev
	r.mu.Unlock()
	if !enabled || data == nil {
		return models
	}
	out := make([]Model, len(models))
	if strings.HasPrefix(providerType, "ollama-") {
		for i, model := range models {
			out[i] = enrichModel(model, ollamaLookup(data, model.ID), providerType)
		}
		return out
	}
	key, ok := modelsDevProviderKey[providerType]
	if !ok {
		return models
	}
	provider, ok := data[key]
	if !ok {
		return models
	}
	for i, model := range models {
		out[i] = enrichModel(model, provider.Models[model.ID], providerType)
	}
	return out
}

// parseModelsDevReasoningOptions converts raw models.dev reasoning_options
// entries into normalized ReasoningOption values. Each entry is parsed
// independently: a malformed entry is skipped while valid siblings are kept.
// Returns nil when every non-empty entry is malformed. A non-nil result with
// an empty Options list means the source explicitly reported an empty list,
// which models.dev defines as no caller-controlled selector.
func parseModelsDevReasoningOptions(raw []map[string]any) *ReasoningCapabilities {
	if len(raw) == 0 {
		return &ReasoningCapabilities{Options: []ReasoningOption{}}
	}
	var options []ReasoningOption
	for _, entry := range raw {
		rawType, _ := entry["type"].(string)
		switch rawType {
		case "effort":
			if vals, ok := entry["values"].([]any); ok {
				var efforts []string
				for _, v := range vals {
					if s, ok := v.(string); ok {
						efforts = append(efforts, s)
					}
				}
				if len(efforts) > 0 {
					options = append(options, ReasoningOption{Type: ReasoningOptionEffort, Values: SortEfforts(efforts)})
				}
			}
		case "toggle":
			// Retain the toggle mechanism without inventing defaults.
			options = append(options, ReasoningOption{Type: ReasoningOptionToggle})
		case "budget_tokens":
			var opt ReasoningOption
			opt.Type = ReasoningOptionBudgetTokens
			if min, ok := CoerceInt64(entry["min"]); ok {
				opt.Min = &min
			}
			if max, ok := CoerceInt64(entry["max"]); ok {
				opt.Max = &max
			}
			options = append(options, opt)
		}
	}
	if len(options) == 0 {
		return nil
	}
	return &ReasoningCapabilities{Options: options}
}

// enrichModel fills the gaps in a single model from its models.dev entry.
func enrichModel(model Model, md modelsDevModel, providerType string) Model {
	if model.ContextLength == 0 && md.Limit.Context > 0 {
		model.ContextLength = md.Limit.Context
	}
	if model.MaxOutputTokens == 0 && md.Limit.Output > 0 {
		model.MaxOutputTokens = md.Limit.Output
	}
	if model.SupportsTools == nil && md.ToolCall != nil {
		model.SupportsTools = md.ToolCall
	}
	if model.SupportsVision == nil {
		if v, ok := visionFromModalities(md.Modalities.Input); ok {
			model.SupportsVision = &v
		}
	}
	if model.SupportsReasoning == nil && md.Reasoning != nil {
		model.SupportsReasoning = md.Reasoning
	}
	if model.SupportsStructuredOutput == nil && md.StructuredOutput != nil {
		model.SupportsStructuredOutput = md.StructuredOutput
	}
	if len(model.InputModalities) == 0 && len(md.Modalities.Input) > 0 {
		model.InputModalities = md.Modalities.Input
	}
	if len(model.OutputModalities) == 0 && len(md.Modalities.Output) > 0 {
		model.OutputModalities = md.Modalities.Output
	}
	var mdReasoning *ReasoningCapabilities
	if md.ReasoningOptions != nil {
		mdReasoning = parseModelsDevReasoningOptions(*md.ReasoningOptions)
	}
	if model.ReasoningCapabilities == nil {
		model.ReasoningCapabilities = mdReasoning
	} else if mdReasoning != nil && !gatewayProviderTypes[providerType] && (model.ReasoningCapabilities.Options == nil || len(model.ReasoningCapabilities.Options) > 0) {
		model.ReasoningCapabilities = mergeReasoningCapabilitiesFallback(model.ReasoningCapabilities, mdReasoning)
	}
	return model
}

// mergeReasoningCapabilitiesFallback fills missing selector mechanisms from
// models.dev while keeping provider metadata authoritative for mechanisms it
// already reported. An explicitly empty provider option list is left alone.
func mergeReasoningCapabilitiesFallback(provider, fallback *ReasoningCapabilities) *ReasoningCapabilities {
	result := *provider
	result.Options = append([]ReasoningOption(nil), provider.Options...)
	if provider.Options == nil && fallback.Options != nil {
		result.Options = []ReasoningOption{}
	}
	present := make(map[ReasoningOptionType]bool)
	for _, option := range result.Options {
		present[option.Type] = true
	}
	for _, option := range fallback.Options {
		if !present[option.Type] {
			result.Options = append(result.Options, option)
		}
	}
	if len(result.ThinkingModes) == 0 {
		result.ThinkingModes = append([]string(nil), fallback.ThinkingModes...)
	}
	if result.DefaultEffort == "" {
		result.DefaultEffort = fallback.DefaultEffort
	}
	if result.Mandatory == nil {
		result.Mandatory = fallback.Mandatory
	}
	if result.DefaultEnabled == nil {
		result.DefaultEnabled = fallback.DefaultEnabled
	}
	if len(result.Parameters) == 0 {
		result.Parameters = append([]string(nil), fallback.Parameters...)
	}
	return &result
}

// CoerceInt64 converts a JSON number or string to int64 without going
// through float. Returns (0, false) when the value is not a clean integer.
func CoerceInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n >= 0 && n < float64(uint64(1)<<63) && n == float64(int64(n)) {
			return int64(n), true
		}
	case string:
		var parsed int64
		if _, err := fmt.Sscanf(n, "%d", &parsed); err == nil && parsed >= 0 {
			return parsed, true
		}
	}
	return 0, false
}

// visionFromModalities derives the vision capability from the models.dev input
// modalities. It reports (false, false) when the provider reports no input
// modalities (vision stays unknown); otherwise it reports whether "image" is
// among them.
func visionFromModalities(input []string) (bool, bool) {
	if len(input) == 0 {
		return false, false
	}
	return slices.Contains(input, "image"), true
}

// LoadModelsDevCache synchronously loads the cached models.dev dataset from
// path. A missing or unreadable cache is not an error: the registry simply
// proceeds without enrichment until a background refresh succeeds.
func (r *Registry) LoadModelsDevCache(path string) {
	data, err := loadModelsDevFile(path)
	if err != nil {
		return
	}
	r.mu.Lock()
	r.modelsDev = data
	r.mu.Unlock()
}

// RefreshModelsDev fetches the models.dev dataset, writes it to the cache file
// at path, and updates the in-memory copy. On any failure the previous in-memory
// and on-disk state is preserved (graceful degradation).
func (r *Registry) RefreshModelsDev(ctx context.Context, path string) error {
	body, err := r.fetchModelsDev(ctx)
	if err != nil {
		return err
	}
	data, err := parseModelsDev(body)
	if err != nil {
		return err
	}
	if err := writeModelsDevFile(path, body); err != nil {
		return err
	}
	r.mu.Lock()
	r.modelsDev = data
	r.mu.Unlock()
	return nil
}

// RefreshModelsDevIfStale refreshes the models.dev cache in the background if
// the cached copy is missing or older than a day. It is a best-effort hook used
// alongside a manual catalogue refresh so a fresh provider refresh also picks up
// fresh models.dev metadata.
func (r *Registry) RefreshModelsDevIfStale(ctx context.Context, path string) {
	go func() {
		refreshCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		_ = r.refreshModelsDevIfStale(refreshCtx, path)
	}()
}

// StartModelsDevRefresh runs a background goroutine that refreshes the models.dev
// cache daily. It first refreshes immediately if the cache is missing or stale.
func (r *Registry) StartModelsDevRefresh(ctx context.Context, path string) {
	go func() {
		initialCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_ = r.refreshModelsDevIfStale(initialCtx, path)
		cancel()
		ticker := time.NewTicker(modelsDevRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				_ = r.refreshModelsDevIfStale(refreshCtx, path)
				cancel()
			}
		}
	}()
}

func (r *Registry) refreshModelsDevIfStale(ctx context.Context, path string) error {
	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < modelsDevMaxAge {
		return nil
	}
	return r.RefreshModelsDev(ctx, path)
}

func (r *Registry) fetchModelsDev(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models.dev returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func parseModelsDev(body []byte) (modelsDevDataset, error) {
	var data modelsDevDataset
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func loadModelsDevFile(path string) (modelsDevDataset, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseModelsDev(body)
}

func writeModelsDevFile(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
