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
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

func cooldownTestHarness(t *testing.T, upstreamA, upstreamB http.HandlerFunc) (*testAPI, string, string, *Server) {
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

	providerIDs := map[string]string{}
	modelIDs := map[string]string{}
	for _, p := range []struct{ name, url string }{{"provider-a", serverA.URL + "/v1"}, {"provider-b", serverB.URL + "/v1"}} {
		status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": p.name, "type": "generic-openai", "base_url": p.url, "credential": "provider-secret"})
		if status != 201 {
			t.Fatalf("create provider %s: %d %v", p.name, status, payload)
		}
		providerIDs[p.name] = payload["id"].(string)
		status, payload, _ = api.request("GET", "/api/admin/providers/"+providerIDs[p.name]+"/models", nil)
		if status != 200 {
			t.Fatal(payload)
		}
		for _, raw := range payload["data"].([]any) {
			m := raw.(map[string]any)
			modelIDs[m["upstream_model_id"].(string)] = m["id"].(string)
		}
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "cooldown client", "type": "catalogue"})
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
	return api, clientSecret, "virtual/coding", app
}

func TestCooldown500SkipsTargetOnNextRequest(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "a")
		mu.Unlock()
		http.Error(w, "fail", 500)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "b")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical, _ := cooldownTestHarness(t, failA, okB)

	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("first request should succeed via B, got %d", resp.StatusCode)
	}

	mu.Lock()
	got := append([]string(nil), reached...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("first request attempt order = %v, want [a b]", got)
	}

	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("second request should succeed, got %d", resp.StatusCode)
	}
	mu.Lock()
	got = append([]string(nil), reached...)
	mu.Unlock()
	if len(got) != 3 || got[2] != "b" {
		t.Fatalf("second request should skip A, got %v", got)
	}
}

func TestCooldownExpiryRetriesTarget(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "a")
		mu.Unlock()
		http.Error(w, "fail", 500)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "b")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical, app := cooldownTestHarness(t, failA, okB)
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{"fallback_cooldown_seconds": 1})
	if status != 204 {
		t.Fatalf("set cooldown: %d", status)
	}

	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("first request should succeed, got %d", resp.StatusCode)
	}

	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("second request should skip A, got %d", resp.StatusCode)
	}

	// Deterministically expire the cooldown instead of waiting for real time:
	// backdate the store entry so the next request retries A.
	var modelA string
	if err := app.db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelA); err != nil {
		t.Fatalf("lookup model-a: %v", err)
	}
	app.cooldown.set(modelA, time.Now().Add(-time.Millisecond), time.Now().Add(-time.Millisecond), "", "", "", "", "")

	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("third request after expiry should retry A, got %d", resp.StatusCode)
	}
	mu.Lock()
	got := append([]string(nil), reached...)
	mu.Unlock()
	countA := 0
	for _, g := range got {
		if g == "a" {
			countA++
		}
	}
	if countA < 2 {
		t.Fatalf("expected A to be tried twice after expiry, got %v", got)
	}
}

func TestCooldownSharedAcrossVirtualModels(t *testing.T) {
	var aCalls atomic.Int32
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		aCalls.Add(1)
		http.Error(w, "fail", 500)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical, _ := cooldownTestHarness(t, failA, okB)

	status, payload, _ := api.request("GET", "/api/admin/virtual-models", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	virtuals := payload["data"].([]any)
	var modelAID, modelBID string
	for _, raw := range virtuals {
		v := raw.(map[string]any)
		targets := v["targets"].([]any)
		for _, tgt := range targets {
			t := tgt.(map[string]any)
			if t["upstream_model_id"] == "model-a" {
				modelAID = t["provider_model_id"].(string)
			}
			if t["upstream_model_id"] == "model-b" {
				modelBID = t["provider_model_id"].(string)
			}
		}
	}
	if modelAID == "" || modelBID == "" {
		t.Fatalf("could not find model IDs: a=%s b=%s", modelAID, modelBID)
	}

	status, payload, _ = api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual2"})
	if status != 201 {
		t.Fatalf("group2: %d %v", status, payload)
	}
	group2ID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{
		"group_id":     group2ID,
		"name":         "general",
		"routing_mode": "ordered_fallback",
		"targets": []any{
			map[string]any{"provider_model_id": modelAID, "enabled": true},
			map[string]any{"provider_model_id": modelBID, "enabled": true},
		},
	})
	if status != 201 {
		t.Fatalf("virtual2: %d %v", status, payload)
	}
	generalID := payload["id"].(string)

	status, payload, _ = api.request("GET", "/api/admin/client-keys", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var clientID string
	for _, raw := range payload["data"].([]any) {
		c := raw.(map[string]any)
		if c["name"] == "cooldown client" {
			clientID = c["id"].(string)
		}
	}
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{
		"defaults": []any{},
		"permissions": []any{
			map[string]any{"kind": "virtual", "model_id": virtuals[0].(map[string]any)["id"].(string), "enabled": true},
			map[string]any{"kind": "virtual", "model_id": generalID, "enabled": true},
		},
	})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}

	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("first request should succeed, got %d", resp.StatusCode)
	}
	if aCalls.Load() != 1 {
		t.Fatalf("A should be tried once, got %d", aCalls.Load())
	}

	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "virtual2/general", "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("second request should succeed, got %d", resp.StatusCode)
	}
	if aCalls.Load() != 1 {
		t.Fatalf("A should be skipped for second virtual model due to cooldown, got %d", aCalls.Load())
	}
}

func TestCooldownDisabledRetriesEveryTime(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "a")
		mu.Unlock()
		http.Error(w, "fail", 500)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "b")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical, _ := cooldownTestHarness(t, failA, okB)
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{"fallback_cooldown_seconds": 0})
	if status != 204 {
		t.Fatalf("disable cooldown: %d", status)
	}

	for i := 0; i < 3; i++ {
		resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
		if resp.StatusCode != 200 {
			t.Fatalf("request %d should succeed, got %d", i, resp.StatusCode)
		}
	}
	mu.Lock()
	got := append([]string(nil), reached...)
	mu.Unlock()
	countA := 0
	for _, g := range got {
		if g == "a" {
			countA++
		}
	}
	if countA != 3 {
		t.Fatalf("with cooldown disabled, A should be tried every time, got %v", got)
	}
}

func TestCooldownRequestSpecificFailureDoesNotCool(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "a")
		mu.Unlock()
		http.Error(w, "bad request", 400)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "b")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical, _ := cooldownTestHarness(t, failA, okB)

	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("first request should succeed via B, got %d", resp.StatusCode)
	}

	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("second request should succeed, got %d", resp.StatusCode)
	}
	mu.Lock()
	got := append([]string(nil), reached...)
	mu.Unlock()
	countA := 0
	for _, g := range got {
		if g == "a" {
			countA++
		}
	}
	if countA != 2 {
		t.Fatalf("400 should not cool A, expected A tried twice, got %v", got)
	}
}

func TestCooldownExhaustionBypass(t *testing.T) {
	var mu sync.Mutex
	var aCalls atomic.Int32
	var bCalls atomic.Int32
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		aCalls.Add(1)
		http.Error(w, "fail", 500)
	})
	failB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		bCalls.Add(1)
		mu.Lock()
		mu.Unlock()
		http.Error(w, "fail", 500)
	})
	api, secret, canonical, _ := cooldownTestHarness(t, failA, failB)

	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 503 {
		t.Fatalf("all targets failing should return 503, got %d", resp.StatusCode)
	}
	if aCalls.Load() != 1 {
		t.Fatalf("A should be tried once in pass 1, got %d", aCalls.Load())
	}
	if bCalls.Load() != 1 {
		t.Fatalf("B should be tried once in pass 1, got %d", bCalls.Load())
	}

	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 503 {
		t.Fatalf("second request should also fail, got %d", resp.StatusCode)
	}
	if aCalls.Load() != 2 {
		t.Fatalf("A should be tried once in pass 1 and once in pass 2, got %d", aCalls.Load())
	}
	if bCalls.Load() != 2 {
		t.Fatalf("B should be tried once in pass 1 and once in pass 2, got %d", bCalls.Load())
	}
}

func TestCooldownDirectRouteUnaffected(t *testing.T) {
	var aCalls atomic.Int32
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		aCalls.Add(1)
		http.Error(w, "fail", 500)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, _, app := cooldownTestHarness(t, failA, okB)

	status, payload, _ := api.request("GET", "/api/admin/providers", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var providerID string
	for _, raw := range payload["data"].([]any) {
		p := raw.(map[string]any)
		if p["name"] == "provider-a" {
			providerID = p["id"].(string)
		}
	}
	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var modelID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["upstream_model_id"] == "model-a" {
			modelID = m["id"].(string)
		}
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "direct client", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	directSecret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{
		"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}},
	})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}

	resp, _ := clientCall(t, api.base, directSecret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{}})
	if resp.StatusCode != 500 {
		t.Fatalf("direct request should fail with 500, got %d", resp.StatusCode)
	}
	resp, _ = clientCall(t, api.base, directSecret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{}})
	if resp.StatusCode != 500 {
		t.Fatalf("second direct request should also fail, got %d", resp.StatusCode)
	}
	if aCalls.Load() != 2 {
		t.Fatalf("direct requests should not be affected by cooldown, got %d calls", aCalls.Load())
	}
	_ = app
	_ = secret
}

func TestCooldownTimeoutOpensCooldown(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	// A hangs (never sends a response header) so the router's per-attempt
	// time-to-first-header bound fires and classifies the failure as
	// upstream_timeout, which must open cooldown. The handler blocks on a
	// channel so the httptest server can shut down cleanly at cleanup.
	releaseA := make(chan struct{})
	hangA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "a")
		mu.Unlock()
		<-releaseA
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "b")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical, app := cooldownTestHarness(t, hangA, okB)
	t.Cleanup(func() { close(releaseA) })
	// Shorten the router's per-attempt time-to-first-header bound so the hang
	// is classified as upstream_timeout quickly instead of waiting 60s.
	app.providers.Registry().SetResponseHeaderTimeout(100 * time.Millisecond)

	apiClient := &http.Client{Timeout: 3 * time.Second}
	body, _ := json.Marshal(map[string]any{"model": canonical, "messages": []any{}})
	req, _ := http.NewRequest("POST", api.base+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("first request should succeed via B, got %d", resp.StatusCode)
	}

	resp2, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp2.StatusCode != 200 {
		t.Fatalf("second request should succeed, got %d", resp2.StatusCode)
	}
	mu.Lock()
	got := append([]string(nil), reached...)
	mu.Unlock()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "b" {
		t.Fatalf("timeout should open cooldown, got %v", got)
	}
}

func TestCooldownResponseTooLargeDoesNotOpen(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	tooLargeA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "a")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.CopyN(w, repeatingByteReader{value: 'x'}, maxUpstreamNonStreamBytes+1)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "b")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical, _ := cooldownTestHarness(t, tooLargeA, okB)

	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("first request should succeed via B, got %d", resp.StatusCode)
	}

	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("second request should succeed, got %d", resp.StatusCode)
	}
	mu.Lock()
	got := append([]string(nil), reached...)
	mu.Unlock()
	countA := 0
	for _, g := range got {
		if g == "a" {
			countA++
		}
	}
	if countA != 2 {
		t.Fatalf("response_too_large should not open cooldown, got %v", got)
	}
}

func TestCooldownRestartClearsState(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "a")
		mu.Unlock()
		http.Error(w, "fail", 500)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "b")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical, app := cooldownTestHarness(t, failA, okB)

	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("first request should succeed, got %d", resp.StatusCode)
	}

	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("second request should skip A, got %d", resp.StatusCode)
	}
	mu.Lock()
	got := append([]string(nil), reached...)
	mu.Unlock()
	if len(got) != 3 || got[2] != "b" {
		t.Fatalf("second request should skip A, got %v", got)
	}

	newApp := newTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8082"}, app.db)
	newRouter := httptest.NewServer(newApp.Handler())
	t.Cleanup(newRouter.Close)

	body, _ := json.Marshal(map[string]any{"model": canonical, "messages": []any{}})
	req, _ := http.NewRequest("POST", newRouter.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("request after restart should succeed, got %d", resp.StatusCode)
	}
	mu.Lock()
	got = append([]string(nil), reached...)
	mu.Unlock()
	if len(got) != 5 || got[3] != "a" || got[4] != "b" {
		t.Fatalf("after restart A should be retried, got %v", got)
	}
}

// TestCooldownDoesNotApplyToFixedVirtualRoute verifies that a failure through a
// FIXED virtual model does not populate the shared cooldown state for the real
// target, so a subsequent ordered-fallback route over the same target is not
// skipped.
func TestCooldownDoesNotApplyToFixedVirtualRoute(t *testing.T) {
	var aCalls atomic.Int32
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		aCalls.Add(1)
		http.Error(w, "fail", 500)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, _, app := cooldownTestHarness(t, failA, okB)

	// Create a FIXED virtual model over provider-a/model-a only.
	var modelA string
	if err := app.db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelA); err != nil {
		t.Fatal(err)
	}
	status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "fixed"})
	if status != 201 {
		t.Fatalf("group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "single", "routing_mode": "fixed", "targets": []any{map[string]any{"provider_model_id": modelA, "enabled": true}}})
	if status != 201 {
		t.Fatalf("fixed virtual: %d %v", status, payload)
	}
	fixedVirtualID := payload["id"].(string)
	// The harness's client key id is not returned; look it up.
	var clientKeyID string
	if err := app.db.SQL.QueryRow(`SELECT id FROM client_keys WHERE name='cooldown client'`).Scan(&clientKeyID); err != nil {
		t.Fatal(err)
	}
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientKeyID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "virtual", "model_id": fixedVirtualID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}

	// First fixed call fails (5xx upstream, surfaced as 503 since a fixed
	// route has no fallback). It must NOT record cooldown for model-a.
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "fixed/single", "messages": []any{}})
	if resp.StatusCode != 503 {
		t.Fatalf("fixed call should fail with 503, got %d", resp.StatusCode)
	}
	// Second fixed call must still hit A (no cooldown).
	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "fixed/single", "messages": []any{}})
	if resp.StatusCode != 503 {
		t.Fatalf("second fixed call should fail with 503, got %d", resp.StatusCode)
	}
	if aCalls.Load() != 2 {
		t.Fatalf("fixed virtual should not be affected by cooldown, got %d calls to A", aCalls.Load())
	}
}

// TestCooldownDoesNotApplyToDirectRealRoute verifies that a direct real-model
// route never populates cooldown, so the same target used by an ordered-fallback
// route is not skipped.
func TestCooldownDoesNotApplyToDirectRealRoute(t *testing.T) {
	var aCalls atomic.Int32
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		aCalls.Add(1)
		http.Error(w, "fail", 500)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, _, app := cooldownTestHarness(t, failA, okB)

	var modelA string
	if err := app.db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelA); err != nil {
		t.Fatal(err)
	}
	// Direct real-model client key.
	status, payload, _ := api.request("POST", "/api/admin/client-keys", map[string]any{"name": "direct", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	directID := payload["id"].(string)
	directSecret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+directID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelA, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}

	resp, _ := clientCall(t, api.base, directSecret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{}})
	if resp.StatusCode != 500 {
		t.Fatalf("direct call should fail with 500, got %d", resp.StatusCode)
	}
	resp, _ = clientCall(t, api.base, directSecret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{}})
	if resp.StatusCode != 500 {
		t.Fatalf("second direct call should fail with 500, got %d", resp.StatusCode)
	}
	if aCalls.Load() != 2 {
		t.Fatalf("direct real route should not be affected by cooldown, got %d calls to A", aCalls.Load())
	}
	_ = secret
}
