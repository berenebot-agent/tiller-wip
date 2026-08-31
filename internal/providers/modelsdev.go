package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// models.dev is an open-source, community-maintained registry of model metadata
// (context length, max output, capability flags, modalities). Tiller uses it as
// a *fallback* only: provider-reported data stays the source of truth, and
// models.dev fills in only the fields the provider left unknown. A field stays
// unknown if neither source reports it (tri-state semantics preserved).

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
	"openrouter":   "openrouter",
	"deepseek":     "deepseek",
	"nvidia-nim":   "nvidia",
	"zai":          "zhipuai",
	"gemini":       "google",
	"alibaba-qwen": "alibaba",
	"fireworks":    "fireworks-ai",
	"azure-openai": "azure",
	"opencode-zen": "opencode",
	"opencode-go":  "opencode",
	"openai":       "openai",
	"anthropic":    "anthropic",
	"groq":         "groq",
	"mistral":      "mistral",
	"xai":          "xai",
	"cerebras":     "cerebras",
	"perplexity":   "perplexity",
	"minimax":      "minimax",
	"huggingface":  "huggingface",
}

// enrich merges models.dev capability metadata into the discovered models for a
// provider. It fills in only the fields the provider left unknown and never
// overrides a provider-reported value. It is a no-op when models.dev is disabled
// or the dataset is unavailable.
func (r *Registry) enrich(models []Model, providerType string) []Model {
	r.mu.Lock()
	enabled := r.modelsDevEnabled
	data := r.modelsDev
	r.mu.Unlock()
	if !enabled || data == nil {
		return models
	}
	key, ok := modelsDevProviderKey[providerType]
	if !ok {
		return models
	}
	provider, ok := data[key]
	if !ok {
		return models
	}
	out := make([]Model, len(models))
	for i, model := range models {
		out[i] = enrichModel(model, provider.Models[model.ID])
	}
	return out
}

// enrichModel fills the gaps in a single model from its models.dev entry.
func enrichModel(model Model, md modelsDevModel) Model {
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
	return model
}

// visionFromModalities derives the vision capability from the models.dev input
// modalities. It reports (false, false) when the provider reports no input
// modalities (vision stays unknown); otherwise it reports whether "image" is
// among them.
func visionFromModalities(input []string) (bool, bool) {
	if len(input) == 0 {
		return false, false
	}
	return contains(input, "image"), true
}

// SetModelsDevEnabled toggles enrichment. It is safe to call before any
// concurrent Discover/enrich calls.
func (r *Registry) SetModelsDevEnabled(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modelsDevEnabled = enabled
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
