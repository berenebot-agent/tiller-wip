package server

import (
	"encoding/json"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestNormalizeCodexRequestResolvesEffortAliases(t *testing.T) {
	aliasCaps := &providers.ReasoningCapabilities{
		Options:       []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "high", "max", "ultra"}}},
		EffortAliases: map[string]string{"ultra": "max"},
	}
	cases := []struct {
		name       string
		body       string
		caps       *providers.ReasoningCapabilities
		wantEffort string
		wantAbsent bool
	}{
		{name: "ultra resolves to max", body: `{"reasoning":{"effort":"ultra"}}`, caps: aliasCaps, wantEffort: "max"},
		{name: "supported effort untouched", body: `{"reasoning":{"effort":"high"}}`, caps: aliasCaps, wantEffort: "high"},
		{name: "persistent dropped", body: `{"reasoning":{"effort":"persistent"}}`, caps: aliasCaps, wantAbsent: true},
		{name: "nil caps leaves effort", body: `{"reasoning":{"effort":"ultra"}}`, caps: nil, wantEffort: "ultra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := normalizeCodexRequest([]byte(tc.body), tc.caps)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(result, &body); err != nil {
				t.Fatal(err)
			}
			reasoning, _ := body["reasoning"].(map[string]any)
			got, _ := reasoning["effort"].(string)
			if tc.wantAbsent {
				if got != "" {
					t.Fatalf("expected effort removed, got %q in %s", got, result)
				}
				if reasoning != nil {
					if _, exists := reasoning["effort"]; exists {
						t.Fatalf("effort still present in %s", result)
					}
				}
				return
			}
			if got != tc.wantEffort {
				t.Fatalf("effort = %q, want %q in %s", got, tc.wantEffort, result)
			}
		})
	}
}
