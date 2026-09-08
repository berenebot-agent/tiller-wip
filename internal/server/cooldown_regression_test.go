package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestCooldownSlowFailureStillCools(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	slowA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "a")
		mu.Unlock()
		time.Sleep(2500 * time.Millisecond)
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
	api, secret, canonical, app := cooldownTestHarness(t, slowA, okB)
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{"fallback_cooldown_seconds": 2})
	if status != 204 {
		t.Fatalf("set cooldown: %d", status)
	}

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
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "b" {
		t.Fatalf("slow failure must still cool A, got %v", got)
	}

	var modelA string
	if err := app.db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelA); err != nil {
		t.Fatalf("lookup model-a: %v", err)
	}
	if !app.cooldown.cooled(modelA, time.Now()) {
		t.Fatal("A should still be cooling immediately after the slow failure")
	}
	entry, ok := app.cooldown.statusByName("provider-a", "model-a", time.Now())
	if !ok {
		t.Fatal("cooldown entry for A should be live")
	}
	if age := time.Since(entry.startedAt); age >= 2*time.Second {
		t.Fatalf("cooldown startedAt age = %v, want under 2s (failure time, not attempt start)", age)
	}
}

func TestCooldownStatusEligibilityMatrix(t *testing.T) {
	cases := []struct {
		status   int
		wantCool bool
	}{
		{400, true},
		{404, true},
		{408, true},
		{429, true},
		{500, true},
		{503, true},
		{405, false},
		{409, false},
		{413, false},
		{415, false},
		{422, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("http_%d", tc.status), func(t *testing.T) {
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
				http.Error(w, "fail", tc.status)
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
			if tc.wantCool {
				if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "b" {
					t.Fatalf("HTTP %d should cool A, got %v", tc.status, got)
				}
				return
			}
			if len(got) != 4 || got[0] != "a" || got[1] != "b" || got[2] != "a" || got[3] != "b" {
				t.Fatalf("HTTP %d must not cool A, got %v", tc.status, got)
			}
		})
	}
}

func TestCooldownEligibleStatusDoesNotApplyToFixedVirtualRoute(t *testing.T) {
	var aCalls int
	var mu sync.Mutex
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		aCalls++
		mu.Unlock()
		http.Error(w, "bad request", 400)
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
	var clientKeyID string
	if err := app.db.SQL.QueryRow(`SELECT id FROM client_keys WHERE name='cooldown client'`).Scan(&clientKeyID); err != nil {
		t.Fatal(err)
	}
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientKeyID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "virtual", "model_id": fixedVirtualID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}

	for i := 0; i < 2; i++ {
		resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "fixed/single", "messages": []any{}})
		if resp.StatusCode != 503 {
			t.Fatalf("fixed call %d should fail with 503, got %d", i+1, resp.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if aCalls != 2 {
		t.Fatalf("fixed virtual 400 must not populate shared cooldown, got %d calls to A", aCalls)
	}
}

func TestCooldownExcludedStatusDoesNotApplyToDirectRealRoute(t *testing.T) {
	var aCalls int
	var mu sync.Mutex
	failA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		mu.Lock()
		aCalls++
		mu.Unlock()
		http.Error(w, "method not allowed", 405)
	})
	okB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, _, _, app := cooldownTestHarness(t, failA, okB)

	var modelA string
	if err := app.db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelA); err != nil {
		t.Fatal(err)
	}
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

	for i := 0; i < 2; i++ {
		resp, _ := clientCall(t, api.base, directSecret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{}})
		if resp.StatusCode != 405 {
			t.Fatalf("direct call %d should fail with 405, got %d", i+1, resp.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if aCalls != 2 {
		t.Fatalf("direct real route must not be affected by cooldown, got %d calls to A", aCalls)
	}
}

func TestCooldownClientCancelDoesNotCool(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	releaseA := make(chan struct{})
	waitA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	api, secret, canonical, app := cooldownTestHarness(t, waitA, okB)
	t.Cleanup(func() { close(releaseA) })
	app.providers.Registry().SetResponseHeaderTimeout(80 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{"model": canonical, "messages": []any{}})
	req, _ := http.NewRequestWithContext(ctx, "POST", api.base+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	var resp *http.Response
	var reqErr error
	go func() {
		resp, reqErr = http.DefaultClient.Do(req)
		close(done)
	}()
	time.Sleep(18 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled request did not return")
	}
	_ = resp
	_ = reqErr

	var modelA string
	if err := app.db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelA); err != nil {
		t.Fatalf("lookup model-a: %v", err)
	}
	if app.cooldown.cooled(modelA, time.Now()) {
		t.Fatal("client cancellation must not globally cool model A")
	}

	resp2, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp2.StatusCode != 200 {
		t.Fatalf("subsequent normal request should succeed via B after non-cooling cancel, got %d", resp2.StatusCode)
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
		t.Fatalf("model A must still be attempted after a cancelled request, hits=%v", got)
	}
}
