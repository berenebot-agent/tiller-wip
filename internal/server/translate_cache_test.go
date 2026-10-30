package server

import (
	"encoding/json"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

type cacheBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl json.RawMessage `json:"cache_control"`
}

type cacheMessage struct {
	Role    string       `json:"role"`
	Content []cacheBlock `json:"content"`
}

type cacheRequest struct {
	System   []cacheBlock   `json:"system"`
	Messages []cacheMessage `json:"messages"`
}

func translateForCache(t *testing.T, messages []any) cacheRequest {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": "m", "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	out, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolMessages, "m")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var parsed cacheRequest
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}

func marked(block cacheBlock) bool {
	return len(block.CacheControl) > 0 && string(block.CacheControl) != "null"
}

// A chat-shape agent loop cannot express an Anthropic cache breakpoint, so the
// translation must add them: one on the last system block (which also covers the
// tool definitions ahead of it) and one closing the transcript, or every turn
// re-bills the whole conversation at full input price.
func TestChatToMessagesInjectsPromptCacheBreakpoints(t *testing.T) {
	req := translateForCache(t, []any{
		map[string]any{"role": "system", "content": "you are a router"},
		map[string]any{"role": "user", "content": "first turn"},
		map[string]any{"role": "assistant", "content": "reply"},
		map[string]any{"role": "user", "content": "second turn"},
	})
	if len(req.System) != 1 || !marked(req.System[0]) {
		t.Fatalf("system breakpoint missing: %+v", req.System)
	}
	last := req.Messages[len(req.Messages)-1]
	if len(last.Content) == 0 || !marked(last.Content[len(last.Content)-1]) {
		t.Fatalf("transcript breakpoint missing: %+v", req.Messages)
	}
	total := 0
	for _, block := range req.System {
		if marked(block) {
			total++
		}
	}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if marked(block) {
				total++
			}
		}
	}
	if total != 2 {
		t.Fatalf("expected exactly 2 breakpoints, got %d", total)
	}
}

// A single-message conversation has no earlier prefix to hit; marking it would
// only pay the cache-write surcharge. The system prefix is still worth caching.
func TestChatToMessagesSkipsTranscriptBreakpointForSingleMessage(t *testing.T) {
	req := translateForCache(t, []any{
		map[string]any{"role": "system", "content": "you are a router"},
		map[string]any{"role": "user", "content": "one shot"},
	})
	if len(req.System) != 1 || !marked(req.System[0]) {
		t.Fatalf("system breakpoint missing: %+v", req.System)
	}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if marked(block) {
				t.Fatalf("unexpected transcript breakpoint: %+v", req.Messages)
			}
		}
	}
}

// The tail of an agent turn is usually a tool result; that block must carry the
// marker, because it is the prefix the next turn extends.
func TestChatToMessagesCachesToolResultTail(t *testing.T) {
	req := translateForCache(t, []any{
		map[string]any{"role": "system", "content": "you are a router"},
		map[string]any{"role": "user", "content": "run the tool"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "c1", "function": map[string]any{"name": "f", "arguments": `{"a":1}`}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "result"},
	})
	last := req.Messages[len(req.Messages)-1]
	if len(last.Content) != 1 || last.Content[0].Type != "tool_result" {
		t.Fatalf("expected a tool_result tail, got %+v", last.Content)
	}
	if !marked(last.Content[0]) {
		t.Fatalf("tool_result tail not marked: %+v", last.Content[0])
	}
}

// Thinking blocks reject cache_control, so the marker must fall back to the
// deepest block that accepts one rather than being placed on the tail blindly.
func TestPromptCacheBreakpointsSkipThinkingBlocks(t *testing.T) {
	systems := []any{map[string]any{"type": "text", "text": "sys"}}
	messages := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "hello"},
		}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "pondering", "signature": "sig"},
		}},
	}
	applyPromptCacheBreakpoints(systems, messages)
	if systems[0].(map[string]any)["cache_control"] == nil {
		t.Fatal("system breakpoint missing")
	}
	thinking := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if thinking["cache_control"] != nil {
		t.Fatal("cache_control placed on a thinking block")
	}
	earlier := messages[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if earlier["cache_control"] == nil {
		t.Fatal("breakpoint fell back past every cacheable block")
	}
}

// The cap is provider-side: once four markers are present the router must add
// none of its own.
func TestPromptCacheBreakpointsRespectProviderCap(t *testing.T) {
	systems := []any{map[string]any{"type": "text", "text": "sys"}}
	messages := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "a", "cache_control": ephemeralCacheControl()},
			map[string]any{"type": "text", "text": "b", "cache_control": ephemeralCacheControl()},
		}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "c", "cache_control": ephemeralCacheControl()},
			map[string]any{"type": "text", "text": "d", "cache_control": ephemeralCacheControl()},
		}},
	}
	applyPromptCacheBreakpoints(systems, messages)
	if systems[0].(map[string]any)["cache_control"] != nil {
		t.Fatal("router added a fifth breakpoint")
	}
	if countCacheBreakpoints(messages) != 4 {
		t.Fatalf("expected the original 4 breakpoints, got %d", countCacheBreakpoints(messages))
	}
}

// A native Messages caller owns its own caching strategy: the same-protocol path
// relays the body untouched.
func TestNativeMessagesRequestIsNotRewritten(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-5-5",
		"max_tokens": 64,
		"system":     "plain string system",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := translateRequest(body, providers.ProtocolMessages, providers.ProtocolMessages, "m")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if string(out) != string(body) {
		t.Fatalf("native Messages body was rewritten:\n%s", out)
	}
}
