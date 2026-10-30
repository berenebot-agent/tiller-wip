package server

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

// anthropicCacheStream is a Messages stream whose input_tokens (10) excludes
// the 90 cache-read and 20 cache-creation tokens, as Anthropic reports them.
const anthropicCacheStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":90,\"cache_creation_input_tokens\":20}}}\n\n" +
	"event: content_block_delta\ndata: {\"delta\":{\"text\":\"hi\"}}\n\n" +
	"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5,\"cache_read_input_tokens\":90}}\n\n" +
	"event: message_stop\ndata: {}\n\n"

// A Chat client sees prompt_tokens include cached input on every frame, since
// it replaces the usage object rather than accumulating it.
func TestTranslatedChatStreamUsageIncludesAnthropicCache(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := translateSSE(rec, nil, bufio.NewReader(strings.NewReader(anthropicCacheStream)), providers.ProtocolChat, providers.ProtocolMessages, "client-model", &usageCapture{}); err != nil {
		t.Fatalf("translateSSE: %v", err)
	}
	last := ""
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, `data: {"choices":[],`) {
			last = line
		}
	}
	// 10 uncached + 90 read + 20 written prompt, 5 output.
	if !strings.Contains(last, `"prompt_tokens":120`) || !strings.Contains(last, `"total_tokens":125`) || !strings.Contains(last, `"completion_tokens":5`) {
		t.Fatalf("final chat usage frame = %s, want prompt_tokens 120 / total 125 / completion 5", last)
	}
}

// A Responses client reads usage from the completed event; it must count the
// whole prompt and report the cached share.
func TestTranslatedResponsesStreamUsageIncludesAnthropicCache(t *testing.T) {
	t.Parallel()
	state := &streamState{id: "m1", model: "m", reasoningIndex: -1, messageIndex: -1, toolIndex: -1}
	state.accumulateUsage(map[string]any{"input_tokens": float64(10), "cache_read_input_tokens": float64(90), "cache_creation_input_tokens": float64(20)})
	state.accumulateUsage(map[string]any{"output_tokens": float64(5)})
	rec := httptest.NewRecorder()
	writeStreamDone(rec, providers.ProtocolResponses, state)
	var completed map[string]any
	for _, frame := range sseFrames(t, rec.Body.String()) {
		if response, ok := frame["response"].(map[string]any); ok {
			completed = response
		}
	}
	u, _ := completed["usage"].(map[string]any)
	if u["input_tokens"] != float64(120) || u["total_tokens"] != float64(125) || u["output_tokens"] != float64(5) {
		t.Fatalf("responses completed usage = %v, want input_tokens=120 total_tokens=125 output_tokens=5", u)
	}
}

// The aggregated (stream:false upstream) and non-streaming converters must
// count cached input in prompt_tokens too.
func TestAnthropicUsageConvertersIncludeCacheTokens(t *testing.T) {
	t.Parallel()
	state := &streamState{id: "r1", model: "m", reasoningIndex: -1, messageIndex: -1, toolIndex: -1}
	aggregated := state.aggregatedNonstreamChat(providers.ProtocolChat, "m", "stop", &usageCapture{inputTokens: int64Ptr(10), outputTokens: int64Ptr(5), cacheReadInputTokens: int64Ptr(90), cacheCreationInputTokens: int64Ptr(20)})
	if u := aggregated["usage"].(map[string]any); u["prompt_tokens"] != int64(120) || u["total_tokens"] != int64(125) {
		t.Fatalf("aggregated usage = %v, want prompt_tokens=120 total_tokens=125", u)
	}

	body, err := json.Marshal(map[string]any{
		"id": "m1", "model": "upstream", "stop_reason": "end_turn",
		"content": []any{map[string]any{"type": "text", "text": "hi"}},
		"usage":   map[string]any{"input_tokens": float64(10), "cache_read_input_tokens": float64(90), "cache_creation_input_tokens": float64(20)},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := translateNonstreamResponse(body, providers.ProtocolChat, providers.ProtocolMessages, "m")
	if err != nil {
		t.Fatalf("translateNonstreamResponse: %v", err)
	}
	var translated map[string]any
	if err := json.Unmarshal(out, &translated); err != nil {
		t.Fatal(err)
	}
	if u := translated["usage"].(map[string]any); u["prompt_tokens"] != float64(120) {
		t.Fatalf("nonstream usage = %v, want prompt_tokens=120 (cached input included)", u)
	}
}
