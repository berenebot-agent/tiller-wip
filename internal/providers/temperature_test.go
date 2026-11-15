package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTemperatureEnrichment(t *testing.T) {
	t.Parallel()
	var data modelsDevDataset
	if err := json.Unmarshal([]byte(`{"anthropic":{"models":{"no-sampling":{"temperature":false},"sampling":{"temperature":true},"unknown":{}}}}`), &data); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, id       string
		provider, want *bool
		disabled       bool
	}{
		{name: "unsupported", id: "no-sampling", want: boolPtr(false)},
		{name: "supported", id: "sampling", want: boolPtr(true)},
		{name: "missing flag", id: "unknown"},
		{name: "missing model", id: "absent"},
		{name: "provider true wins", id: "no-sampling", provider: boolPtr(true), want: boolPtr(true)},
		{name: "provider false wins", id: "sampling", provider: boolPtr(false), want: boolPtr(false)},
		{name: "disabled enrichment", id: "no-sampling", disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			r.modelsDev = data
			r.modelsDevEnabled = !tc.disabled
			models := r.enrich([]Model{{ID: tc.id, SupportsTemperature: tc.provider}}, "claude-subscription")
			got := toCatalogueModels(models)[0].SupportsTemperature
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("temperature = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiscoverTemperatureParameters(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"a","supported_parameters":["temperature"]},{"id":"b","supported_parameters":["tools"]},{"id":"c","supported_parameters":[]},{"id":"d"},{"id":"e","supported_parameters":null}]}`))
	}))
	defer upstream.Close()
	// Exercise both the paginated catalogue and Command Code's endpoint parser.
	for _, providerType := range []string{"anthropic", "commandcode"} {
		t.Run(providerType, func(t *testing.T) {
			models, err := NewRegistry().Discover(context.Background(), Instance{Type: providerType, BaseURL: upstream.URL})
			if err != nil {
				t.Fatal(err)
			}
			want := []*bool{boolPtr(true), boolPtr(false), boolPtr(false), nil, nil}
			if len(models) != len(want) {
				t.Fatalf("model count = %d", len(models))
			}
			for i, m := range models {
				got := m.SupportsTemperature
				if (got == nil) != (want[i] == nil) || got != nil && *got != *want[i] {
					t.Errorf("model %s temperature = %v, want %v", m.ID, got, want[i])
				}
			}
		})
	}
}
