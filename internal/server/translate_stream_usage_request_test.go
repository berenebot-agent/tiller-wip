package server

import (
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestRequestChatStreamUsage(t *testing.T) {
	t.Parallel()
	stream := `{"model":"m","stream":true}`
	for _, tc := range []struct {
		name, body, provider string
		target               providers.Protocol
		wantAdded            bool
	}{
		{"openai chat stream", stream, "openai", providers.ProtocolChat, true},
		{"azure chat stream", stream, "azure-openai", providers.ProtocolChat, true},
		{"other provider untouched", stream, "generic-openai", providers.ProtocolChat, false},
		{"non-stream untouched", `{"model":"m","stream":false}`, "openai", providers.ProtocolChat, false},
		{"other target untouched", stream, "openai", providers.ProtocolResponses, false},
		{"client options kept", `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`, "openai", providers.ProtocolChat, false},
	} {
		got := string(requestChatStreamUsage([]byte(tc.body), tc.target, tc.provider))
		if added := strings.Contains(got, `"include_usage":true`); added != tc.wantAdded {
			t.Errorf("%s: include_usage added = %v, want %v (%s)", tc.name, added, tc.wantAdded, got)
		}
	}
}
