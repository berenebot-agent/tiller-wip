package providers

import (
	"math"
)

// Cost rates are published per one million tokens in USD. Estimation consumes
// them only for display; a provider-reported cost (e.g. OpenRouter's
// usage.cost) is authoritative when present.

// costUSDPerToken converts a per-million-token rate to USD per token.
func costUSDPerToken(ratePerMillion float64) float64 {
	return ratePerMillion / 1_000_000
}

// CostRates is a model's resolved per-token pricing in USD. A nil field means
// that rate is unknown for the model.
type CostRates struct {
	Input      *float64
	Output     *float64
	CacheRead  *float64
	CacheWrite *float64
}

// ratesFromModelsDev extracts base per-token rates from a models.dev cost
// block. Tiers are deliberately ignored here (they are context-dependent);
// base rates are the conservative default.
func ratesFromModelsDev(c modelsDevCost) CostRates {
	var r CostRates
	if c.Input != nil {
		v := costUSDPerToken(*c.Input)
		r.Input = &v
	}
	if c.Output != nil {
		v := costUSDPerToken(*c.Output)
		r.Output = &v
	}
	if c.CacheRead != nil {
		v := costUSDPerToken(*c.CacheRead)
		r.CacheRead = &v
	}
	if c.CacheWrite != nil {
		v := costUSDPerToken(*c.CacheWrite)
		r.CacheWrite = &v
	}
	return r
}

// lookupCostRates returns the base cost rates for a provider type + model ID,
// using the same provider→lab and model resolution the metadata enrichment
// uses (exact key for most providers, vendor-lab for Copilot, inferred lab for
// Ollama). Returns (rates, true) only when at least one rate is known.
func (r *Registry) lookupCostRates(providerType, modelID string) (CostRates, bool) {
	r.mu.Lock()
	enabled := r.modelsDevEnabled
	data := r.modelsDev
	r.mu.Unlock()
	if !enabled || data == nil || modelID == "" {
		return CostRates{}, false
	}
	var md modelsDevModel
	switch {
	case providerType == "ollama-cloud":
		md = data["ollama-cloud"].Models[modelID]
	case providerType == "ollama-local":
		return CostRates{}, false
	case providerType == "github-copilot":
		// The Copilot catalogue carries a per-model vendor label, but the cost
		// lookup runs from a log row that only has the upstream model ID, so
		// try each vendor lab by exact ID. A miss is a no-op (no cost shown).
		md = data["github-copilot"].Models[modelID]
		if md.Cost.Input == nil && md.Cost.Output == nil {
			md = copilotCostLookup(data, modelID)
		}
	default:
		key, ok := modelsDevProviderKey[providerType]
		switch providerType {
		case "together":
			key, ok = "togetherai", true
		case "cloudflare-ai":
			key, ok = "cloudflare-workers-ai", true
		case "bedrock-api-key":
			key, ok = "amazon-bedrock", true
		case "opencode-go":
			key, ok = "opencode-go", true
		}
		if !ok {
			return CostRates{}, false
		}
		provider, ok := data[key]
		if !ok {
			return CostRates{}, false
		}
		md = provider.Models[modelID]
	}
	rates := ratesFromModelsDev(md.Cost)
	if rates.Input == nil && rates.Output == nil && rates.CacheRead == nil && rates.CacheWrite == nil {
		return CostRates{}, false
	}
	return rates, true
}

// copilotCostLookup resolves cost rates for a Copilot model by trying each
// mapped vendor lab with the exact model ID. It returns the first lab that has
// a cost block for the ID.
func copilotCostLookup(data modelsDevDataset, id string) modelsDevModel {
	for _, lab := range copilotVendorLab {
		provider, ok := data[lab]
		if !ok {
			continue
		}
		if m, ok := provider.Models[id]; ok {
			if m.Cost.Input != nil || m.Cost.Output != nil || m.Cost.CacheRead != nil || m.Cost.CacheWrite != nil {
				return m
			}
		}
	}
	return modelsDevModel{}
}

// EstimatedCostMicros returns the estimated cost of a request in micro-dollars
// (1e-6 USD) using the model's published rates. providerType is the provider
// type (e.g. "openai"), not the display name. Returns (costMicros, true) when
// at least one rate applies; the caller decides whether to store it. Cache
// tokens are billed at the cache-read rate when known and fall back to the
// input rate otherwise; cache-write tokens are billed at the cache-write rate
// when known. Cache token counts are treated as a subset of input tokens only
// Anthropic reports uncached input separately. OpenAI-compatible totals already
// include cache tokens, so those subsets are subtracted before pricing input.
func (r *Registry) EstimatedCostMicros(providerType, modelID string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int64) (int64, bool) {
	rates, ok := r.lookupCostRates(providerType, modelID)
	if !ok {
		return 0, false
	}
	var usd float64
	if providerType != "anthropic" && providerType != "claude-subscription" {
		inputTokens = max(0, inputTokens-cacheReadTokens-cacheCreationTokens)
	}
	if rates.Input != nil {
		usd += float64(inputTokens) * *rates.Input
	}
	if rates.Output != nil {
		usd += float64(outputTokens) * *rates.Output
	}
	if cacheReadTokens > 0 {
		rate := rates.CacheRead
		if rate == nil {
			rate = rates.Input
		}
		if rate != nil {
			usd += float64(cacheReadTokens) * *rate
		}
	}
	if cacheCreationTokens > 0 {
		rate := rates.CacheWrite
		if rate == nil {
			rate = rates.Input
		}
		if rate != nil {
			usd += float64(cacheCreationTokens) * *rate
		}
	}
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd*1_000_000 >= math.MaxInt64 {
		return 0, false
	}
	if usd <= 0 {
		return 0, true
	}
	return int64(math.Round(usd * 1_000_000)), true
}
