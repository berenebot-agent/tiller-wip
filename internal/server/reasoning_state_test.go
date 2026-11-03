package server

import (
	"encoding/json"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestReasoningDetailsAnthropicRoundTrip(t *testing.T) {
	t.Parallel()
	details := []any{map[string]any{"type": "reasoning.text", "text": "", "signature": "sig", "format": "anthropic-claude-v1", "index": 2}, map[string]any{"type": "reasoning.encrypted", "data": "opaque", "format": "anthropic-claude-v1"}}
	blocks, err := chatReasoningDetailsToAnthropic(details)
	if err != nil {
		t.Fatal(err)
	}
	if blocks[0].(map[string]any)["signature"] != "sig" || blocks[1].(map[string]any)["data"] != "opaque" {
		t.Fatalf("blocks lost state: %#v", blocks)
	}
	chat := map[string]any{"model": "x", "messages": []any{map[string]any{"role": "assistant", "content": "", "reasoning_details": details}}}
	body, err := json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	translated, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolMessages, "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(translated); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesReasoningDetailsRoundTrip(t *testing.T) {
	t.Parallel()
	item := map[string]any{"type": "reasoning", "id": "rs_1", "encrypted_content": "opaque", "summary": []any{map[string]any{"type": "summary_text", "text": "plan"}}}
	details := responsesReasoningToChatDetails(item)
	if len(details) != 2 {
		t.Fatalf("details = %#v", details)
	}
	details = append(details, map[string]any{"type": "reasoning.summary", "summary": "second", "format": "openai-responses-v1", "id": "rs_1"})
	items, err := chatReasoningDetailsToResponses(details)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].(map[string]any)["encrypted_content"] != "opaque" || len(items[0].(map[string]any)["summary"].([]any)) != 2 {
		t.Fatalf("items = %#v", items)
	}
}

func TestPlaintextResponsesSummaryIsRepresentable(t *testing.T) {
	t.Parallel()
	item := map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "plan"}}}
	if got := responsesReasoningToChatDetails(item); len(got) != 1 {
		t.Fatalf("details = %#v", got)
	}
	if _, err := chatReasoningDetailsToResponses([]any{map[string]any{"type": "reasoning.summary", "summary": "plan", "format": "openai-responses-v1"}}); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesReasoningDegradedForMessages(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"x","input":[{"type":"reasoning","encrypted_content":"opaque"},{"type":"message","role":"user","content":"hello"}]}`)
	out, stripped, err := translateRequestObserved(body, providers.ProtocolResponses, providers.ProtocolMessages, "claude-x")
	if err != nil || !stripped {
		t.Fatalf("expected observable degradation, stripped=%v err=%v", stripped, err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	messages := result["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
		t.Fatalf("reasoning-only turn must be omitted: %v", result)
	}
}
