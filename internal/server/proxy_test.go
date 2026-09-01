package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestPreflightResponseLimitRejectsOverflowWithoutTruncatingAcceptedBodies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "below", body: "123"},
		{name: "exact", body: "1234"},
		{name: "over", body: "12345", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				Header: make(http.Header),
				Body:   io.NopCloser(bytes.NewBufferString(tc.body)),
			}
			err := preflightResponseLimit(resp, 4)
			if tc.wantErr {
				if !errors.Is(err, errUpstreamResponseTooLarge) {
					t.Fatalf("expected oversized response error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.body {
				t.Fatalf("accepted response changed: got %q want %q", got, tc.body)
			}
		})
	}
}

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

func TestCrossProtocolStatefulFeatureReturnsMachineReadableError(t *testing.T) {
	api, _, _, secret := loggingTestHarness(t, mockUpstream(t))
	resp, payload := clientCall(t, api.base, secret, "/v1/responses", map[string]any{
		"model":                "provider-a/model-a",
		"input":                "hello",
		"previous_response_id": "resp_previous",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; payload=%v", resp.StatusCode, payload)
	}
	errPayload, _ := payload["error"].(map[string]any)
	if errPayload["code"] != "unsupported_feature" {
		t.Fatalf("error code = %v, want unsupported_feature; payload=%v", errPayload["code"], payload)
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
