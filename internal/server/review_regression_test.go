package server

import (
	"encoding/json"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestMessagesDegradationWithoutToolCalls(t *testing.T) {
	t.Parallel()
	for _, history := range []string{
		`{"role":"assistant","content":"answer","reasoning_details":{ "data":"opaque"}}`,
		`{"role":"assistant","content":"answer","reasoning_content":"private","reasoning_details":[]}`,
		`{"role":"assistant","content":"answer","reasoning_content":"private"}`,
		`{"role":"assistant","content":"answer","reasoning_details":[{"type":"reasoning.encrypted","format":"foreign","data":"opaque"}]}`,
	} {
		out, stripped, err := translateRequestObserved([]byte(`{"messages":[`+history+`,{"role":"user","content":"next"}]}`), providers.ProtocolChat, providers.ProtocolMessages, "target")
		if err != nil || !stripped {
			t.Fatalf("degradation not reported: stripped=%v err=%v", stripped, err)
		}
		var result map[string]any
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		messages := result["messages"].([]any)
		if len(messages) != 2 || len(messages[0].(map[string]any)["content"].([]any)) != 1 {
			t.Fatal("visible conversation was not retained")
		}
	}
}

func TestChatUsageCacheDetailsPresence(t *testing.T) {
	t.Parallel()
	state := &streamState{}
	if _, ok := state.chatUsageDelta()["prompt_tokens_details"]; ok {
		t.Fatal("absent cache usage must stay absent")
	}
	state.accumulateUsage(map[string]any{"cache_read_input_tokens": float64(0)})
	details, ok := state.chatUsageDelta()["prompt_tokens_details"].(map[string]any)
	if !ok || details["cached_tokens"] != int64(0) {
		t.Fatal("explicit zero cache usage was lost")
	}
}
