package server

import (
	"encoding/json"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

// Anthropic rejects empty and whitespace-only text blocks, so a tool-only
// assistant turn arriving as content "" must still translate to a valid
// tool_use-only message.
func TestChatToMessagesDropsBlankTextBlocks(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "c1", "function": map[string]any{"name": "f", "arguments": `{"a":1}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
			map[string]any{"role": "assistant", "content": "  \n "},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "   "},
				map[string]any{"type": "text", "text": "keep"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolMessages, "m")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var parsed struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Messages) != 5 {
		t.Fatalf("messages = %d, want 5", len(parsed.Messages))
	}
	if got := parsed.Messages[1].Content; len(got) != 1 || got[0].Type != "tool_use" {
		t.Fatalf("tool-only assistant turn = %+v, want only tool_use", got)
	}
	if got := parsed.Messages[3].Content; len(got) != 0 {
		t.Fatalf("whitespace assistant turn = %+v, want empty", got)
	}
	if got := parsed.Messages[4].Content; len(got) != 1 || got[0].Text != "keep" {
		t.Fatalf("user blocks = %+v, want only the non-blank text", got)
	}
}
