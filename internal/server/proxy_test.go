package server

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestCompatibleProtocolPrefersNativeModelProtocol(t *testing.T) {
	if got := compatibleProtocol([]providers.Protocol{providers.ProtocolChat, providers.ProtocolResponses, providers.ProtocolMessages}, providers.ProtocolResponses, providers.ProtocolChat); got != providers.ProtocolResponses {
		t.Fatalf("native protocol was not selected: got %q", got)
	}
	if got := compatibleProtocol([]providers.Protocol{providers.ProtocolChat, providers.ProtocolResponses, providers.ProtocolMessages}, providers.ProtocolChat, providers.ProtocolResponses); got != providers.ProtocolChat {
		t.Fatalf("native Chat protocol was not selected: got %q", got)
	}
	if got := compatibleProtocol([]providers.Protocol{providers.ProtocolChat}, "", providers.ProtocolResponses); got != providers.ProtocolChat {
		t.Fatalf("provider translation protocol was not selected: got %q", got)
	}
}

func TestCrossProtocolResponsesRejectsStatefulFeatures(t *testing.T) {
	for _, field := range []string{"conversation", "previous_response_id", "store", "background", "files"} {
		body := []byte(`{"model":"virtual/coding","input":"hello","` + field + `":"value"}`)
		_, err := translateRequest(body, providers.ProtocolResponses, providers.ProtocolChat, "real-model")
		var unsupported unsupportedFeature
		if !errors.As(err, &unsupported) {
			t.Fatalf("%s was not rejected with unsupported_feature: %v", field, err)
		}
	}
}

func TestChatMessagesRoundTripPreservesToolIDsAndArguments(t *testing.T) {
	body := []byte(`{"model":"virtual/coding","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call_123","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Perth\"}"}}]},{"role":"tool","tool_call_id":"call_123","content":"sunny"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	messages, err := translateRequest(body, providers.ProtocolChat, providers.ProtocolMessages, "claude-real")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := translateRequest(messages, providers.ProtocolMessages, providers.ProtocolChat, "openai-real")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{`call_123`, `lookup`, `Perth`, `sunny`} {
		if !bytes.Contains(chat, []byte(needle)) {
			t.Fatalf("round trip lost %q: %s", needle, chat)
		}
	}
}

func FuzzProtocolRequestParser(f *testing.F) {
	f.Add([]byte(`{"model":"virtual/coding","messages":[]}`))
	f.Add([]byte(`{"input":"hello"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = translateRequest(body, providers.ProtocolChat, providers.ProtocolMessages, "target")
		_, _ = translateRequest(body, providers.ProtocolResponses, providers.ProtocolChat, "target")
	})
}
