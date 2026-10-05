package providers

import "testing"

func floatPtr(v float64) *float64 { return &v }

func TestEstimatedCostMicrosBasic(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = modelsDevDataset{
		"anthropic": {Models: map[string]modelsDevModel{
			"claude-x": {
				Cost: modelsDevCost{
					Input:      floatPtr(3.0),
					Output:     floatPtr(15.0),
					CacheRead:  floatPtr(0.3),
					CacheWrite: floatPtr(3.75),
				},
			},
		}},
	}
	// 1M input = $3, 1M output = $15, 1M cache-read = $0.30, 1M cache-write = $3.75.
	got, ok := r.EstimatedCostMicros("anthropic", "claude-x", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("expected a cost estimate")
	}
	want := int64(3_000_000 + 15_000_000 + 300_000 + 3_750_000)
	if got != want {
		t.Fatalf("cost micros = %d, want %d", got, want)
	}
}

func TestEstimatedCostMicrosCacheFallsBackToInputRate(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = modelsDevDataset{
		"openai": {Models: map[string]modelsDevModel{
			"gpt-x": {Cost: modelsDevCost{Input: floatPtr(2.0), Output: floatPtr(8.0)}},
		}},
	}
	// No cache rate: cache tokens bill at the input rate.
	got, ok := r.EstimatedCostMicros("openai", "gpt-x", 0, 0, 1_000_000, 0)
	if !ok {
		t.Fatal("expected a cost estimate")
	}
	if got != 2_000_000 {
		t.Fatalf("cost micros = %d, want 2000000", got)
	}
}

func TestEstimatedCostMicrosUnknownModel(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = modelsDevDataset{"openai": {Models: map[string]modelsDevModel{}}}
	if _, ok := r.EstimatedCostMicros("openai", "missing", 100, 100, 0, 0); ok {
		t.Fatal("expected no estimate for an unknown model")
	}
}

func TestEstimatedCostMicrosDisabled(t *testing.T) {
	r := NewRegistry()
	r.modelsDevEnabled = false
	r.modelsDev = modelsDevDataset{"openai": {Models: map[string]modelsDevModel{
		"gpt-x": {Cost: modelsDevCost{Input: floatPtr(1.0)}},
	}}}
	if _, ok := r.EstimatedCostMicros("openai", "gpt-x", 1000, 0, 0, 0); ok {
		t.Fatal("expected no estimate when models.dev is disabled")
	}
}

func TestEstimatedCostMicrosSuppressesSubscriptionProviders(t *testing.T) {
	r := NewRegistry()
	r.modelsDev = modelsDevDataset{
		"openai": {Models: map[string]modelsDevModel{
			"gpt-x": {Cost: modelsDevCost{Input: floatPtr(3.0)}},
		}},
		"anthropic": {Models: map[string]modelsDevModel{
			"claude-x": {Cost: modelsDevCost{Input: floatPtr(3.0)}},
		}},
		"github-copilot": {Models: map[string]modelsDevModel{
			"copilot-model": {Cost: modelsDevCost{Input: floatPtr(3.0)}},
		}},
	}
	for _, test := range []struct{ providerType, modelID string }{
		{"codex-subscription", "gpt-x"},
		{"claude-subscription", "claude-x"},
		{"github-copilot", "copilot-model"},
	} {
		t.Run(test.providerType, func(t *testing.T) {
			if _, ok := r.EstimatedCostMicros(test.providerType, test.modelID, 1_000_000, 0, 0, 0); ok {
				t.Fatal("expected no per-token estimate for subscription provider")
			}
		})
	}
}

func TestParseModelsDevCost(t *testing.T) {
	body := []byte(`{"openai":{"models":{"gpt-x":{"cost":{"input":1.5,"output":6,"cache_read":0.15}}}}}`)
	data, err := parseModelsDev(body)
	if err != nil {
		t.Fatal(err)
	}
	c := data["openai"].Models["gpt-x"].Cost
	if c.Input == nil || *c.Input != 1.5 || c.Output == nil || *c.Output != 6 || c.CacheRead == nil || *c.CacheRead != 0.15 {
		t.Fatalf("parsed cost = %+v", c)
	}
}
