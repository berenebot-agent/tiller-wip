package server

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeDeepSeekRequestMapsToggleToThinkingType(t *testing.T) {
	t.Parallel()
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

func TestNormalizeDeepSeekRequestBackfillsReasoningContent(t *testing.T) {
	t.Parallel()
	tools := `[{"type":"function","function":{"name":"get_time"}}]`
	noTools := `[]`
	cases := []struct {
		name          string
		body          string
		wantAnnotated int // assistant turns expected to gain reasoning_content
	}{
		{
			name:          "tools with assistant turns missing reasoning_content are backfilled",
			body:          `{"model":"x","tools":` + tools + `,"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"tool","content":"r"}]}`,
			wantAnnotated: 1,
		},
		{
			name:          "tools with tool_calls turn missing reasoning_content are backfilled",
			body:          `{"model":"x","tools":` + tools + `,"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1"}]}]}`,
			wantAnnotated: 1,
		},
		{
			name:          "no tools is left untouched",
			body:          `{"model":"x","tools":` + noTools + `,"messages":[{"role":"assistant","content":"b"}]}`,
			wantAnnotated: 0,
		},
		{
			name:          "absent tools is left untouched",
			body:          `{"model":"x","messages":[{"role":"assistant","content":"b"}]}`,
			wantAnnotated: 0,
		},
		{
			name:          "existing reasoning_content is preserved",
			body:          `{"model":"x","tools":` + tools + `,"messages":[{"role":"assistant","content":"b","reasoning_content":"kept"}]}`,
			wantAnnotated: 0,
		},
		{
			name:          "existing empty reasoning_content counts as present",
			body:          `{"model":"x","tools":` + tools + `,"messages":[{"role":"assistant","content":"b","reasoning_content":""}]}`,
			wantAnnotated: 0,
		},
		{
			name:          "foreign reasoning_details still needs native placeholder",
			body:          `{"model":"x","tools":` + tools + `,"messages":[{"role":"assistant","content":"b","reasoning_details":[{"type":"reasoning.text"}]}]}`,
			wantAnnotated: 1,
		},
		{
			name:          "text reasoning details need native placeholder",
			body:          `{"tools":` + tools + `,"messages":[{"role":"assistant","reasoning_details":[{"type":"reasoning.text","text":"foreign"}]}]}`,
			wantAnnotated: 1,
		},
		{
			name:          "opaque reasoning details need native placeholder",
			body:          `{"tools":` + tools + `,"messages":[{"role":"assistant","reasoning_details":[{"type":"reasoning.encrypted","data":"opaque"}]}]}`,
			wantAnnotated: 1,
		},
		{
			name: "native reasoning with foreign details is preserved",
			body: `{"tools":` + tools + `,"messages":[{"role":"assistant","reasoning_content":"native","reasoning_details":[{"type":"reasoning.text","text":"foreign"}]}]}`,
		},
		{
			name: "foreign details without tools are untouched",
			body: `{"messages":[{"role":"assistant","reasoning_details":[{"type":"reasoning.text","text":"foreign"}]}]}`,
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
			var before map[string]any
			if err := json.Unmarshal([]byte(tc.body), &before); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(assistantValues(body, "reasoning_details"), assistantValues(before, "reasoning_details")) {
				t.Fatal("foreign reasoning details changed")
			}
			if got, want := assistantValues(body, "reasoning_content"), assistantValues(before, "reasoning_content"); len(got) != len(want)+tc.wantAnnotated {
				t.Fatalf("assistant reasoning_content values %d, want %d (input had %d, backfill %d) in %s", len(got), len(want)+tc.wantAnnotated, len(want), tc.wantAnnotated, result)
			}
			for i, want := range assistantValues(before, "reasoning_content") {
				if assistantValues(body, "reasoning_content")[i] != want {
					t.Fatalf("existing reasoning_content at %d changed from %v to %v in %s", i, want, assistantValues(body, "reasoning_content")[i], result)
				}
			}
			if tc.wantAnnotated > 0 {
				if v := assistantValues(body, "reasoning_content"); v[len(v)-1] != "" {
					t.Fatalf("backfilled reasoning_content = %v, want empty string in %s", v[len(v)-1], result)
				}
			}
		})
	}
}

func assistantValues(body map[string]any, key string) []any {
	messages, _ := body["messages"].([]any)
	values := []any{}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] != "assistant" {
			continue
		}
		if value, exists := message[key]; exists {
			values = append(values, value)
		}
	}
	return values
}

func TestIsReasoningRoundtripError(t *testing.T) {
	t.Parallel()
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
