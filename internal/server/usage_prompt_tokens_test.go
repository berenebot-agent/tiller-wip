package server

import (
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

// TestPromptTokensFollowsUpstreamUsageFormat pins the single normalisation
// rule: Messages reports uncached input (cache read/creation are folded in
// once), Chat and Responses already report the whole prompt.
func TestPromptTokensFollowsUpstreamUsageFormat(t *testing.T) {
	t.Parallel()
	read, creation := int64(90), int64(20)
	cases := []struct {
		name    string
		capture usageCapture
		want    *int64
	}{
		{
			name:    "messages folds cache read and creation in",
			capture: usageCapture{protocol: providers.ProtocolMessages, inputTokens: int64Ptr(10), cacheReadInputTokens: &read, cacheCreationInputTokens: &creation},
			want:    int64Ptr(120),
		},
		{
			name:    "messages without cache counts is unchanged",
			capture: usageCapture{protocol: providers.ProtocolMessages, inputTokens: int64Ptr(10)},
			want:    int64Ptr(10),
		},
		{
			name:    "chat is already inclusive",
			capture: usageCapture{protocol: providers.ProtocolChat, inputTokens: int64Ptr(100), cacheReadInputTokens: &read},
			want:    int64Ptr(100),
		},
		{
			name:    "responses is already inclusive",
			capture: usageCapture{protocol: providers.ProtocolResponses, inputTokens: int64Ptr(100), cacheReadInputTokens: &read},
			want:    int64Ptr(100),
		},
		{
			name:    "no input count stays absent",
			capture: usageCapture{protocol: providers.ProtocolMessages, cacheReadInputTokens: &read},
			want:    nil,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.capture.promptTokens()
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("promptTokens() = %v, want %v", got, tc.want)
			}
			if got != nil && *got != *tc.want {
				t.Fatalf("promptTokens() = %d, want %d", *got, *tc.want)
			}
		})
	}
}

// TestStoredUsageIsNormalisedByUpstreamProtocol drives the real capture path
// for every supported usage shape, then asserts what copyUsage would store.
// The convention follows the upstream format, never the client protocol.
func TestStoredUsageIsNormalisedByUpstreamProtocol(t *testing.T) {
	t.Parallel()

	// Non-streaming Anthropic Messages usage: input_tokens excludes cache.
	var anthropic usageCapture
	anthropic.protocol = providers.ProtocolMessages
	extractUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":90,"cache_creation_input_tokens":20}}`), &anthropic)

	// Streaming Anthropic Messages usage: split across message_start and
	// message_delta, with the cache counts only counting once.
	var anthropicStream usageCapture
	anthropicStream.protocol = providers.ProtocolMessages
	captureStreamUsage(map[string]any{
		"type": "message_start",
		"message": map[string]any{"usage": map[string]any{
			"input_tokens":                float64(10),
			"cache_read_input_tokens":     float64(90),
			"cache_creation_input_tokens": float64(20),
		}},
	}, providers.ProtocolMessages, &anthropicStream)
	captureStreamUsage(map[string]any{
		"type":  "message_delta",
		"usage": map[string]any{"output_tokens": float64(5), "cache_read_input_tokens": float64(90)},
	}, providers.ProtocolMessages, &anthropicStream)

	// OpenAI Chat usage: prompt_tokens already includes the cached tokens.
	var openai usageCapture
	openai.protocol = providers.ProtocolChat
	extractUsage([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":90}}}`), &openai)

	// DeepSeek reports the hit count natively; prompt_tokens is still inclusive.
	var deepseek usageCapture
	deepseek.protocol = providers.ProtocolChat
	extractUsage([]byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":5,"prompt_cache_hit_tokens":800}}`), &deepseek)

	// OpenAI Responses usage: input_tokens already includes the cached tokens.
	var responses usageCapture
	responses.protocol = providers.ProtocolResponses
	extractUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":90}}}`), &responses)

	cases := []struct {
		name      string
		capture   *usageCapture
		wantInput int64
		wantCache int64
	}{
		{"anthropic non-stream", &anthropic, 120, 90},
		{"anthropic stream", &anthropicStream, 120, 90},
		{"openai chat", &openai, 100, 90},
		{"deepseek chat", &deepseek, 1000, 800},
		{"openai responses", &responses, 100, 90},
	}
	for _, tc := range cases {
		row := &logRow{}
		row.copyUsage(tc.capture)
		if row.inputTokens == nil || *row.inputTokens != tc.wantInput {
			t.Fatalf("%s: stored input_tokens = %v, want %d", tc.name, row.inputTokens, tc.wantInput)
		}
		if row.cacheReadInputTokens == nil || *row.cacheReadInputTokens != tc.wantCache {
			t.Fatalf("%s: stored cache_read_input_tokens = %v, want %d", tc.name, row.cacheReadInputTokens, tc.wantCache)
		}
	}
}

// TestAggregatedUsageDoesNotDoubleCountInclusivePrompt is the regression for a
// Messages-only assumption leaking into the Chat intermediate: cache counts are
// only folded in when the upstream format excluded them.
func TestAggregatedUsageDoesNotDoubleCountInclusivePrompt(t *testing.T) {
	t.Parallel()
	read, creation := int64(90), int64(20)
	cases := []struct {
		name    string
		capture usageCapture
		want    int64
	}{
		{
			name:    "messages target folds cache in once",
			capture: usageCapture{protocol: providers.ProtocolMessages, inputTokens: int64Ptr(10), outputTokens: int64Ptr(5), cacheReadInputTokens: &read, cacheCreationInputTokens: &creation},
			want:    120,
		},
		{
			name:    "chat target is already inclusive",
			capture: usageCapture{protocol: providers.ProtocolChat, inputTokens: int64Ptr(100), outputTokens: int64Ptr(5), cacheReadInputTokens: &read},
			want:    100,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := &streamState{id: "r1", model: "m", reasoningIndex: -1, messageIndex: -1, toolIndex: -1}
			got := state.aggregatedNonstreamChat(providers.ProtocolChat, "m", "stop", &tc.capture)
			u := got["usage"].(map[string]any)
			if u["prompt_tokens"] != tc.want {
				t.Fatalf("prompt_tokens = %v, want %d", u["prompt_tokens"], tc.want)
			}
			if u["total_tokens"] != tc.want+5 {
				t.Fatalf("total_tokens = %v, want %d", u["total_tokens"], tc.want+5)
			}
		})
	}
}

// TestChatResponseToMessagesUsageExcludesCache proves a Messages client is not
// told the whole prompt was uncached input when the intermediate carries the
// cache split from a Messages target.
func TestChatResponseToMessagesUsageExcludesCache(t *testing.T) {
	t.Parallel()
	base := func(usage map[string]any) map[string]any {
		return map[string]any{
			"id": "c1", "model": "m",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hi"}, "finish_reason": "stop"}},
			"usage":   usage,
		}
	}
	msg := chatResponseToMessages(base(map[string]any{
		"prompt_tokens": float64(120), "completion_tokens": float64(5),
		"cache_read_input_tokens": float64(90), "cache_creation_input_tokens": float64(20),
	}), "m")
	u := msg["usage"].(map[string]any)
	if u["input_tokens"] != int64(10) {
		t.Fatalf("input_tokens = %v, want 10 (uncached)", u["input_tokens"])
	}
	if u["cache_read_input_tokens"] != float64(90) || u["cache_creation_input_tokens"] != float64(20) {
		t.Fatalf("cache counts not carried through: %v", u)
	}

	// No cache split (e.g. a Chat target): input_tokens stays the prompt.
	msg = chatResponseToMessages(base(map[string]any{"prompt_tokens": float64(100), "completion_tokens": float64(5)}), "m")
	u = msg["usage"].(map[string]any)
	if u["input_tokens"] != int64(100) {
		t.Fatalf("input_tokens = %v, want 100", u["input_tokens"])
	}
}
