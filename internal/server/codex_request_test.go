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

// TestNormalizeCodexRequestRequestsReasoningSummary pins the Codex request shape
// that keeps a long reasoning prefill from being silent: whenever the resolved
// effort is active, the request asks for a reasoning summary and for encrypted
// reasoning content, mirroring codex_cli_rs / 9router. An explicit client
// summary is preserved, and a disabled/absent reasoning object is left alone.
func TestNormalizeCodexRequestRequestsReasoningSummary(t *testing.T) {
	caps := &providers.ReasoningCapabilities{
		Options:       []providers.ReasoningOption{{Type: providers.ReasoningOptionEffort, Values: []string{"low", "high", "max"}}},
		EffortAliases: map[string]string{},
	}
	cases := []struct {
		name           string
		body           string
		wantSummary    bool // whether a summary must be present
		wantSummaryVal string
		wantInclude    bool // whether reasoning.encrypted_content must be present
	}{
		{name: "active effort gains auto summary", body: `{"reasoning":{"effort":"high"}}`, wantSummary: true, wantSummaryVal: "auto", wantInclude: true},
		{name: "explicit summary preserved", body: `{"reasoning":{"effort":"high","summary":"concise"}}`, wantSummary: true, wantSummaryVal: "concise", wantInclude: true},
		{name: "disabled reasoning untouched", body: `{"reasoning":{"effort":"none"}}`, wantSummary: false, wantInclude: false},
		{name: "absent reasoning untouched", body: `{"input":"hi"}`, wantSummary: false, wantInclude: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := normalizeCodexRequest([]byte(tc.body), caps)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(result, &body); err != nil {
				t.Fatal(err)
			}
			reasoning, _ := body["reasoning"].(map[string]any)
			_, hasSummary := reasoning["summary"]
			if hasSummary != tc.wantSummary {
				t.Fatalf("summary present = %v, want %v in %s", hasSummary, tc.wantSummary, result)
			}
			if tc.wantSummary {
				got, _ := reasoning["summary"].(string)
				if got != tc.wantSummaryVal {
					t.Fatalf("summary = %q, want %q in %s", got, tc.wantSummaryVal, result)
				}
			}
			include, _ := body["include"].([]any)
			hasEncrypted := false
			for _, item := range include {
				if entry, ok := item.(string); ok && entry == "reasoning.encrypted_content" {
					hasEncrypted = true
				}
			}
			if hasEncrypted != tc.wantInclude {
				t.Fatalf("include reasoning.encrypted_content = %v, want %v in %s", hasEncrypted, tc.wantInclude, result)
			}
		})
	}
}

// TestEnsureReasoningEncryptedContentDedupes checks the include-list helper
// does not append a duplicate and tolerates a non-list value.
func TestEnsureReasoningEncryptedContentDedupes(t *testing.T) {
	got := ensureReasoningEncryptedContent([]any{"reasoning.encrypted_content"})
	if len(got) != 1 {
		t.Fatalf("duplicate appended: %v", got)
	}
	got = ensureReasoningEncryptedContent(nil)
	if len(got) != 1 || got[0] != "reasoning.encrypted_content" {
		t.Fatalf("fresh list wrong: %v", got)
	}
	got = ensureReasoningEncryptedContent("not-a-list")
	if len(got) != 1 || got[0] != "reasoning.encrypted_content" {
		t.Fatalf("non-list value not replaced: %v", got)
	}
}
