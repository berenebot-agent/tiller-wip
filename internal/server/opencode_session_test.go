package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
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
