package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

func TestOpenCodeSessionIDNamespacing(t *testing.T) {
	first := openCodeSessionID("conv-1", "req-1", "client-key-a")
	again := openCodeSessionID("conv-1", "req-1", "client-key-a")
	if first != again {
		t.Fatalf("same client + same session must be stable: %q vs %q", first, again)
	}
	other := openCodeSessionID("conv-1", "req-1", "client-key-b")
	if first == other {
		t.Fatalf("different clients + same session must differ: both %q", first)
	}
	if len(first) > 128 || len(other) > 128 {
		t.Fatalf("namespaced session must fit the 128-byte upstream cap: %d / %d", len(first), len(other))
	}
}

func TestOpenCodeSessionIDSynthesizedStable(t *testing.T) {
	first := openCodeSessionID("", "req-abc", "client-key-a")
	again := openCodeSessionID("", "req-abc", "client-key-a")
	if first != again {
		t.Fatalf("synthesized session must be stable across fallback attempts: %q vs %q", first, again)
	}
	if first != "tiller-req-abc" {
		t.Fatalf("synthesized session = %q, want tiller-req-abc", first)
	}
	if anon := openCodeSessionID("", "", "client-key-a"); anon != "tiller-anonymous" {
		t.Fatalf("empty request id must stay anonymous-safe, got %q", anon)
	}
}

func TestCodexSessionIDUsesOpenCodeConversation(t *testing.T) {
	first, firstSource := codexSessionID("journey-board", "req-1", "client-key-a")
	again, againSource := codexSessionID("journey-board", "req-2", "client-key-a")
	if first != again {
		t.Fatalf("same client + same OpenCode session must be stable: %q vs %q", first, again)
	}
	if firstSource != "client" || againSource != "client" {
		t.Fatalf("session sources = %q/%q, want client/client", firstSource, againSource)
	}
	other, source := codexSessionID("journey-board", "req-1", "client-key-b")
	if first == other {
		t.Fatalf("different clients + same OpenCode session must differ: both %q", first)
	}
	if source != "client" {
		t.Fatalf("other session source = %q, want client", source)
	}
	fallback, source := codexSessionID("", "req-1", "client-key-a")
	if fallback != "tiller-req-1" || source != "request" {
		t.Fatalf("fallback Codex session = %q/%q, want tiller-req-1/request", fallback, source)
	}
}

func TestClientSessionHeaderPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{name: "opencode session", headers: map[string]string{"X-Opencode-Session": "oc-1"}, want: "oc-1"},
		{name: "session affinity", headers: map[string]string{"x-session-affinity": "aff-1"}, want: "aff-1"},
		{name: "session id", headers: map[string]string{"X-Session-Id": "sid-1"}, want: "sid-1"},
		{name: "opencode wins over affinity", headers: map[string]string{"X-Opencode-Session": "oc-1", "x-session-affinity": "aff-1"}, want: "oc-1"},
		{name: "affinity wins over session id", headers: map[string]string{"x-session-affinity": "aff-1", "X-Session-Id": "sid-1"}, want: "aff-1"},
		{name: "none", headers: map[string]string{}, want: ""},
		{name: "blank is ignored", headers: map[string]string{"X-Session-Id": "   ", "x-session-affinity": "aff-2"}, want: "aff-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for key, value := range tc.headers {
				h.Set(key, value)
			}
			if got := clientSessionHeader(h); got != tc.want {
				t.Fatalf("clientSessionHeader = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCodexSessionAffinityHeaderStable guards the OpenCode-through-Tiller cache
// affinity path: OpenCode sends x-session-affinity / X-Session-Id (not
// x-opencode-session) to third-party providers, and those must map to one
// stable upstream Codex session-id across turns while an absent header keeps
// requests isolated.
func TestCodexSessionAffinityHeaderStable(t *testing.T) {
	var mu sync.Mutex
	sessions := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"slug": "gpt-5.6-sol", "display_name": "gpt-5.6-sol", "supported_in_api": true}}})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "gpt-5.6-sol"}}})
		case "/responses":
			mu.Lock()
			sessions = append(sessions, r.Header.Get("session-id"))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp-1", "object": "response", "model": "gpt-5.6-sol", "output_text": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	app := newTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)

	status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": "codex-mock", "type": "codex-subscription", "base_url": upstream.URL, "protocols": []any{"responses"}})
	if status != 201 {
		t.Fatalf("create provider: %d %v", status, payload)
	}
	providerID := payload["id"].(string)

	future := time.Now().Add(time.Hour)
	putOAuthToken(t, db, oauth.TokenRecord{
		ProviderID: providerID, AccessToken: "live-token", RefreshToken: "refresh-token", TokenType: "Bearer",
		ExpiresAt: &future, AuthState: oauth.AuthConnected, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var modelID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["upstream_model_id"] == "gpt-5.6-sol" {
			modelID = m["id"].(string)
		}
	}
	if modelID == "" {
		t.Fatal("mock upstream did not expose gpt-5.6-sol")
	}

	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "test client", "description": "codex-affinity", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}

	call := func(header, value string) {
		t.Helper()
		encoded, _ := json.Marshal(map[string]any{"model": "codex-mock/gpt-5.6-sol", "input": "hello"})
		req, _ := http.NewRequest("POST", api.base+"/v1/responses", bytes.NewReader(encoded))
		req.Header.Set("Authorization", "Bearer "+clientSecret)
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set(header, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request status = %d", resp.StatusCode)
		}
	}

	call("X-Session-Id", "conv-1")
	call("x-session-affinity", "conv-1")
	call("", "")

	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 3 {
		t.Fatalf("expected 3 upstream requests, got %d", len(sessions))
	}
	if sessions[0] == "" || sessions[1] == "" {
		t.Fatalf("client-affinity requests must carry a session-id, got %v", sessions)
	}
	if sessions[0] != sessions[1] {
		t.Fatalf("same conversation affinity must map to one session-id: %q vs %q", sessions[0], sessions[1])
	}
	if sessions[2] == sessions[0] {
		t.Fatalf("headerless request must stay isolated, but reused %q", sessions[2])
	}
}

func opencodeSessionHarness(t *testing.T, upstreamA, upstreamB http.HandlerFunc) (*testAPI, string, string) {
	t.Helper()
	serverA := httptest.NewServer(upstreamA)
	t.Cleanup(serverA.Close)
	serverB := httptest.NewServer(upstreamB)
	t.Cleanup(serverB.Close)

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)

	modelIDs := map[string]string{}
	for _, p := range []struct{ name, url string }{{"provider-a", serverA.URL + "/v1"}, {"provider-b", serverB.URL + "/v1"}} {
		status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": p.name, "type": "opencode-zen", "base_url": p.url, "credential": "provider-secret"})
		if status != 201 {
			t.Fatalf("create provider %s: %d %v", p.name, status, payload)
		}
		providerID := payload["id"].(string)
		status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
		if status != 200 {
			t.Fatal(payload)
		}
		for _, raw := range payload["data"].([]any) {
			m := raw.(map[string]any)
			modelIDs[m["upstream_model_id"].(string)] = m["id"].(string)
		}
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "opencode client", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "coding", "routing_mode": "ordered_fallback", "targets": []any{
		map[string]any{"provider_model_id": modelIDs["model-a"], "enabled": true},
		map[string]any{"provider_model_id": modelIDs["model-b"], "enabled": true},
	}})
	if status != 201 {
		t.Fatalf("virtual: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "virtual", "model_id": virtualID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}
	return api, clientSecret, "virtual/coding"
}

func opencodeSessionClientCall(t *testing.T, base, secret, opencodeSession string, body any) (*http.Response, map[string]any) {
	t.Helper()
	encoded, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", bytes.NewReader(encoded))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	if opencodeSession != "" {
		req.Header.Set("X-Opencode-Session", opencodeSession)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	decoded := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	resp.Body.Close()
	return resp, decoded
}

func TestOpenCodeSessionHeaderStableAcrossFallback(t *testing.T) {
	var mu sync.Mutex
	sessions := []string{}
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		sessions = append(sessions, r.Header.Get("X-Opencode-Session"))
		mu.Unlock()
		http.Error(w, "fail", 500)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		mu.Lock()
		sessions = append(sessions, r.Header.Get("X-Opencode-Session"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical := opencodeSessionHarness(t, failA, okB)

	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("request should succeed via B, got %d", resp.StatusCode)
	}
	mu.Lock()
	got := append([]string(nil), sessions...)
	mu.Unlock()
	if len(got) != 2 || got[0] == "" || got[0] != got[1] {
		t.Fatalf("fallback attempts must share one synthesized session, got %v", got)
	}
}

func TestOpenCodeSuppliedSessionHeaderStableAcrossFallback(t *testing.T) {
	var mu sync.Mutex
	sessions := []string{}
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		sessions = append(sessions, r.Header.Get("X-Opencode-Session"))
		mu.Unlock()
		http.Error(w, "fail", 500)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		mu.Lock()
		sessions = append(sessions, r.Header.Get("X-Opencode-Session"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical := opencodeSessionHarness(t, failA, okB)

	resp, _ := opencodeSessionClientCall(t, api.base, secret, "conv-7", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("request should succeed via B, got %d", resp.StatusCode)
	}
	resp, _ = opencodeSessionClientCall(t, api.base, secret, "conv-7", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("second request should succeed, got %d", resp.StatusCode)
	}
	mu.Lock()
	got := append([]string(nil), sessions...)
	mu.Unlock()
	if len(got) != 3 || got[0] == "" || got[0] != got[1] || got[0] != got[2] {
		t.Fatalf("supplied session must map to one stable upstream session, got %v", got)
	}
}
