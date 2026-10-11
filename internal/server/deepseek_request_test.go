package server

import (
	"encoding/json"
	"testing"
)

func TestNormalizeDeepSeekRequestMapsToggleToThinkingType(t *testing.T) {
	cases := []struct {
		name                 string
		body                 string
		wantThinking         string
		wantReasoningRemoved bool
		wantEffort           string
	}{
		{
			name:                 "disable toggle becomes thinking disabled",
			body:                 `{"model":"deepseek-flash","reasoning":{"enabled":false}}`,
			wantThinking:         "disabled",
			wantReasoningRemoved: true,
		},
		{
			name:                 "enable toggle becomes thinking enabled",
			body:                 `{"model":"deepseek-flash","reasoning":{"enabled":true}}`,
			wantThinking:         "enabled",
			wantReasoningRemoved: true,
		},
		{
			name:                 "none effort becomes thinking disabled",
			body:                 `{"model":"deepseek-flash","reasoning_effort":"none"}`,
			wantThinking:         "disabled",
			wantReasoningRemoved: false,
		},
		{
			name:         "supported effort untouched",
			body:         `{"model":"deepseek-flash","reasoning_effort":"high"}`,
			wantThinking: "",
			wantEffort:   "high",
		},
		{
			name:         "no reasoning fields untouched",
			body:         `{"model":"deepseek-flash","messages":[]}`,
			wantThinking: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := normalizeDeepSeekRequest([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(result, &body); err != nil {
				t.Fatal(err)
			}
			thinking, _ := body["thinking"].(map[string]any)
			gotThinking, _ := thinking["type"].(string)
			if gotThinking != tc.wantThinking {
				t.Fatalf("thinking.type = %q, want %q in %s", gotThinking, tc.wantThinking, result)
			}
			_, reasoningPresent := body["reasoning"]
			if tc.wantReasoningRemoved && reasoningPresent {
				t.Fatalf("reasoning still present in %s", result)
			}
			gotEffort, _ := body["reasoning_effort"].(string)
			if gotEffort != tc.wantEffort {
				t.Fatalf("reasoning_effort = %q, want %q in %s", gotEffort, tc.wantEffort, result)
			}
		})
	}
}

func TestIsReasoningRoundtripError(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		{"The `reasoning_content` in the thinking mode must be passed back to the API. (request_id: 1dd8d41a)", true},
		{"reasoning_content must be passed back", true},
		{"some unrelated provider error", false},
		{"reasoning_content is required", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isReasoningRoundtripError(upstreamErrorDetail{Message: tc.message}); got != tc.want {
			t.Errorf("isReasoningRoundtripError(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
}
