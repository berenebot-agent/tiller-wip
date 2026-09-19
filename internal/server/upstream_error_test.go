package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestParseUpstreamErrorDetailOpenAI(t *testing.T) {
	body := []byte(`{"error":{"message":"Invalid value: 'ultra'","type":"invalid_request_error","param":"reasoning.effort","code":"invalid_value"}}`)
	got := parseUpstreamErrorDetail(body, "application/json")
	if got.Message != "Invalid value: 'ultra'" || got.Code != "invalid_value" || got.Param != "reasoning.effort" {
		t.Fatalf("unexpected detail: %+v", got)
	}
}

func TestParseUpstreamErrorDetailAnthropic(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"credit balance too low"}}`)
	if got := parseUpstreamErrorDetail(body, "application/json"); got.Message != "credit balance too low" {
		t.Fatalf("unexpected detail: %+v", got)
	}
}

func TestParseUpstreamErrorDetailKeepsInnerType(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}`)
	got := parseUpstreamErrorDetail(body, "application/json")
	if got.Type != "FreeTierError" {
		t.Fatalf("inner type = %q, want FreeTierError: %+v", got.Type, got)
	}
	if got.Message == "" {
		t.Fatalf("message should survive: %+v", got)
	}
}

func TestIsOpenCodeFreeTierRejection(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"console wrapped anthropic shape", `{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}`, true},
		{"openai shape", `{"error":{"type":"FreeTierError","message":"Error from provider (Console): free tier can only be used from within OpenCode"}}`, true},
		{"case insensitive", `{"error":{"type":"freetiererror","message":"FROM WITHIN OPENCODE"}}`, true},
		{"ordinary provider error", `{"error":{"type":"invalid_request_error","message":"credit balance too low"}}`, false},
		{"free tier words without gate", `{"error":{"message":"free tier quota exceeded for today"}}`, false},
		{"gate words without free tier", `{"error":{"message":"must be called from within OpenCode console"}}`, false},
		{"empty", ``, false},
		{"garbage", `<html>502</html>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOpenCodeFreeTierRejection([]byte(tc.body)); got != tc.want {
				t.Fatalf("isOpenCodeFreeTierRejection(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestFreeShadowSessionOpaqueAndStable(t *testing.T) {
	a := freeShadowSession("tiller-req-123")
	b := freeShadowSession("tiller-req-123")
	if a != b || a == "" {
		t.Fatalf("shadow session must be stable and non-empty, got %q vs %q", a, b)
	}
	if strings.Contains(a, "tiller") {
		t.Fatalf("shadow session must not carry the tiller tell: %q", a)
	}
	if !strings.HasPrefix(a, "ses_") {
		t.Fatalf("shadow session must mirror the ses_ shape, got %q", a)
	}
	if c := freeShadowSession("tiller-req-456"); c == a {
		t.Fatalf("distinct sessions must not collide: %q", c)
	}
}

func TestParseUpstreamErrorDetailTolerantForms(t *testing.T) {
	if got := parseUpstreamErrorDetail([]byte(`{"error":{"code":429,"message":"rate limited"}}`), "application/json"); got.Code != "429" || got.Message != "rate limited" {
		t.Fatalf("numeric code: %+v", got)
	}
	if got := parseUpstreamErrorDetail([]byte(`{"error":"plain failure"}`), "application/json"); got.Message != "plain failure" {
		t.Fatalf("string error: %+v", got)
	}
	if got := parseUpstreamErrorDetail([]byte(`{"message":"top level"}`), "application/json"); got.Message != "top level" {
		t.Fatalf("top-level message: %+v", got)
	}
}

func TestParseUpstreamErrorDetailGarbageAndPlain(t *testing.T) {
	if got := parseUpstreamErrorDetail([]byte("<html>502</html>"), "application/json"); got.clientMessage() != "" {
		t.Fatalf("html should not parse: %+v", got)
	}
	if got := parseUpstreamErrorDetail([]byte("provider says no\n"), "text/plain; charset=utf-8"); got.Message != "provider says no" {
		t.Fatalf("plain text: %+v", got)
	}
}

func TestRedactProviderSecrets(t *testing.T) {
	p := providers.Instance{
		Credential:        "sk-secret",
		OAuthAccountID:    "acct-123",
		OAuthProviderData: map[string]any{"copilot_token": "copilot-secret"},
	}
	text := "auth=" + p.Credential + " copilot=" + p.OAuthProviderData["copilot_token"].(string) + " acct=" + p.OAuthAccountID + " ok"
	got := redactProviderSecrets(text, p)
	for _, secret := range []string{"sk-secret", "copilot-secret", "acct-123"} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret %q not redacted: %q", secret, got)
		}
	}
	if !strings.Contains(got, "ok") {
		t.Fatalf("non-secret text removed: %q", got)
	}
}

func TestVirtualExhaustedRouteListsTargetErrors(t *testing.T) {
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "provider A rejected the request", "code": "invalid_value"}})
	})
	failB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "provider B is overloaded"}})
	})
	api, secret, canonical, _ := cooldownTestHarness(t, failA, failB)
	resp, payload := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("exhausted virtual route status = %d, want 503", resp.StatusCode)
	}
	encoded := string(mustJSON(t, payload))
	for _, want := range []string{"provider-a/model-a", "provider-b/model-b", "provider A rejected the request", "provider B is overloaded"} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("exhausted route list missing %q: %v", want, payload)
		}
	}
}

func TestVirtualFallbackSuccessHidesProviderError(t *testing.T) {
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "SHOULD-NOT-REACH-CLIENT"}})
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical, _ := cooldownTestHarness(t, failA, okB)
	resp, payload := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fallback success status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(string(mustJSON(t, payload)), "SHOULD-NOT-REACH-CLIENT") {
		t.Fatalf("failed fallback target leaked into a successful response: %v", payload)
	}
}
