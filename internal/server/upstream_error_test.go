package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/providers"
)

func TestParseUpstreamErrorDetailOpenAI(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":{"message":"Invalid value: 'ultra'","type":"invalid_request_error","param":"reasoning.effort","code":"invalid_value"}}`)
	got := parseUpstreamErrorDetail(body, "application/json")
	if got.Message != "Invalid value: 'ultra'" || got.Code != "invalid_value" || got.Param != "reasoning.effort" {
		t.Fatalf("unexpected detail: %+v", got)
	}
}

func TestParseUpstreamErrorDetailAnthropic(t *testing.T) {
	t.Parallel()
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"credit balance too low"}}`)
	if got := parseUpstreamErrorDetail(body, "application/json"); got.Message != "credit balance too low" {
		t.Fatalf("unexpected detail: %+v", got)
	}
}

func TestParseUpstreamErrorDetailKeepsInnerType(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// wrapNestedError builds a relay-style body whose error.message is the JSON
// text of inner — the double-encoded shape Command Code produces.
func wrapNestedError(t *testing.T, inner map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	outer, err := json.Marshal(map[string]any{"error": map[string]any{
		"message": string(encoded),
		"type":    "invalid_request_error",
	}})
	if err != nil {
		t.Fatalf("marshal outer: %v", err)
	}
	return outer
}

func TestParseUpstreamErrorDetailUnwrapsNestedRelayError(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":{"message":"{\"type\":\"invalid_request_error\",\"code\":\"\",\"message\":\"invalid request error trace_id: dc06d04f3faca50f715815dffe1a6ac5\"}\n","type":"invalid_request_error"}}`)
	got := parseUpstreamErrorDetail(body, "application/json")
	if want := "invalid request error trace_id: dc06d04f3faca50f715815dffe1a6ac5"; got.Message != want {
		t.Fatalf("message = %q, want %q", got.Message, want)
	}
	if got.Type != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error", got.Type)
	}
	if strings.Contains(got.clientMessage(), `"code"`) {
		t.Fatalf("nested blob leaked to the client: %q", got.clientMessage())
	}
}

func TestParseUpstreamErrorDetailUnwrapEnablesContextLimit(t *testing.T) {
	t.Parallel()
	body := wrapNestedError(t, map[string]any{"error": map[string]any{
		"message": "This model's maximum context length is 131072 tokens.",
		"type":    "invalid_request_error",
		"code":    "context_length_exceeded",
	}})
	got := parseUpstreamErrorDetail(body, "application/json")
	if got.Code != "context_length_exceeded" {
		t.Fatalf("code = %q, want context_length_exceeded", got.Code)
	}
	if !isContextLimitError(got) {
		t.Fatalf("context-limit must be classified through a wrap: %+v", got)
	}
}

func TestParseUpstreamErrorDetailUnwrapEnablesReasoningRoundtrip(t *testing.T) {
	t.Parallel()
	body := wrapNestedError(t, map[string]any{"error": map[string]any{
		"message": "The reasoning_content in the thinking mode must be passed back to the API",
		"type":    "invalid_request_error",
	}})
	got := parseUpstreamErrorDetail(body, "application/json")
	if !isReasoningRoundtripError(got) {
		t.Fatalf("reasoning round-trip must be classified through a wrap: %+v", got)
	}
}

func TestParseUpstreamErrorDetailUnwrapDepthCap(t *testing.T) {
	t.Parallel()
	message := "deepest"
	for i := 0; i < 5; i++ {
		encoded, err := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
		if err != nil {
			t.Fatalf("marshal level %d: %v", i, err)
		}
		message = string(encoded)
	}
	got := parseUpstreamErrorDetail([]byte(message), "application/json")
	if !strings.HasPrefix(got.Message, "{") {
		t.Fatalf("depth cap must stop before the innermost level, got %q", got.Message)
	}
}

func TestParseUpstreamErrorDetailUnwrapIgnoresOpaqueJSON(t *testing.T) {
	t.Parallel()
	inner, err := json.Marshal(map[string]any{"foo": "bar"})
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	body, err := json.Marshal(map[string]any{"error": map[string]any{
		"message": string(inner),
		"code":    "invalid_value",
	}})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	got := parseUpstreamErrorDetail(body, "application/json")
	if got.Message != `{"foo":"bar"}` {
		t.Fatalf("opaque JSON must be left intact, got %q", got.Message)
	}
	if got.Code != "invalid_value" {
		t.Fatalf("code = %q, want invalid_value", got.Code)
	}
}

func TestParseUpstreamErrorDetailUnwrapKeepsOuterCode(t *testing.T) {
	t.Parallel()
	body := wrapNestedError(t, map[string]any{"message": "rate limited"})
	got := parseUpstreamErrorDetail(body, "application/json")
	if got.Message != "rate limited" {
		t.Fatalf("message = %q, want rate limited", got.Message)
	}
}

func TestNestedUpstreamErrorIsRedacted(t *testing.T) {
	t.Parallel()
	const secret = "provider-secret"
	body := wrapNestedError(t, map[string]any{"error": map[string]any{
		"message": "rejected; auth=" + secret,
		"type":    "invalid_request_error",
	}})
	detail := parseUpstreamErrorDetail(body, "application/json")
	got := redactProviderSecrets(detail.clientMessage(), providers.Instance{Credential: secret})
	if strings.Contains(got, secret) {
		t.Fatalf("nested credential not redacted: %q", got)
	}
	if !strings.Contains(got, "rejected") {
		t.Fatalf("nested message lost: %q", got)
	}
}

func TestIsContextLimitError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		detail upstreamErrorDetail
		want   bool
	}{
		{name: "structured code", detail: upstreamErrorDetail{Code: "context_length_exceeded"}, want: true},
		{name: "window message", detail: upstreamErrorDetail{Message: "The request exceeds the context window for this model."}, want: true},
		{name: "input message", detail: upstreamErrorDetail{Message: "The input is too long."}, want: true},
		{name: "unrelated validation", detail: upstreamErrorDetail{Message: "Invalid value for reasoning.effort", Code: "invalid_value"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isContextLimitError(tc.detail); got != tc.want {
				t.Fatalf("isContextLimitError(%+v) = %v, want %v", tc.detail, got, tc.want)
			}
		})
	}
}

func TestContextLimitDoesNotTriggerCooldown(t *testing.T) {
	t.Parallel()
	if cooldownTrigger("context_limit_exceeded", http.StatusBadRequest) {
		t.Fatal("context-limit rejection must not cool a target")
	}
}

func TestParseUpstreamErrorDetailGarbageAndPlain(t *testing.T) {
	t.Parallel()
	if got := parseUpstreamErrorDetail([]byte("<html>502</html>"), "application/json"); got.clientMessage() != "" {
		t.Fatalf("html should not parse: %+v", got)
	}
	if got := parseUpstreamErrorDetail([]byte("provider says no\n"), "text/plain; charset=utf-8"); got.Message != "provider says no" {
		t.Fatalf("plain text: %+v", got)
	}
}

func TestRedactProviderSecrets(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
