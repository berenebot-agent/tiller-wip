package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type repeatingByteReader struct{ value byte }

func (r repeatingByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.value
	}
	return len(p), nil
}

func TestOversizedNonStreamResponseFallsBackBeforeOutput(t *testing.T) {
	oversized := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.CopyN(w, repeatingByteReader{value: 'x'}, maxUpstreamNonStreamBytes+1)
	})
	succeeding := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "fallback-ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical := notificationTestHarness(t, oversized, succeeding)
	resp, payload := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != http.StatusOK || payload["id"] != "fallback-ok" {
		t.Fatalf("oversized response did not fall back before output: status=%d payload=%v", resp.StatusCode, payload)
	}
}

func TestOrderedFallbackCoversUpstreamHTTPFailures(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	failing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		status := int(body["test_status"].(float64))
		mu.Lock()
		reached = append(reached, "a")
		mu.Unlock()
		w.WriteHeader(status)
	})
	succeeding := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		mu.Lock()
		reached = append(reached, "b")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-b", "choices": []any{}})
	})
	api, secret, canonical := notificationTestHarness(t, failing, succeeding)

	for _, status := range []int{400, 401, 403, 404, 409, 422, 429, 500, 503} {
		mu.Lock()
		reached = nil
		mu.Unlock()
		resp, payload := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{
			"model": canonical, "messages": []any{}, "test_status": status,
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d did not fall back: response=%d payload=%v", status, resp.StatusCode, payload)
		}
		mu.Lock()
		got := append([]string(nil), reached...)
		mu.Unlock()
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Fatalf("status %d attempt order = %v, want [a b]", status, got)
		}
	}
}

func TestOrderedFallbackDoesNotWaitForStalledErrorBody(t *testing.T) {
	first := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	second := okUpstream(t)
	api, secret, canonical := notificationTestHarness(t, first, second)

	body, _ := json.Marshal(map[string]any{"model": canonical, "messages": []any{}})
	req, _ := http.NewRequest(http.MethodPost, api.base+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := api.client.Do(req)
		if err == nil {
			done <- resp
		}
	}()
	select {
	case resp := <-done:
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stalled error body prevented fallback: status=%d", resp.StatusCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fallback waited for stalled provider error body")
	}
}

func TestOrderedFallbackCoversNetworkAndTLSFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL func(*testing.T) string
	}{
		{name: "network", baseURL: func(*testing.T) string { return "http://127.0.0.1:1/v1" }},
		{name: "tls", baseURL: func(t *testing.T) string {
			server := httptest.NewTLSServer(http.NotFoundHandler())
			t.Cleanup(server.Close)
			return server.URL + "/v1"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, secret, canonical := notificationTestHarness(t, failUpstream(t), okUpstream(t))
			status, providersPayload, _ := api.request(http.MethodGet, "/api/admin/providers", nil)
			if status != http.StatusOK {
				t.Fatalf("list providers: %d %v", status, providersPayload)
			}
			var providerID string
			for _, raw := range providersPayload["data"].([]any) {
				provider := raw.(map[string]any)
				if provider["name"] == "provider-a" {
					providerID = provider["id"].(string)
				}
			}
			status, payload, _ := api.request(http.MethodPatch, "/api/admin/providers/"+providerID, map[string]any{"base_url": tc.baseURL(t)})
			if status != http.StatusNoContent {
				t.Fatalf("update provider: %d %v", status, payload)
			}
			resp, payload := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
			if resp.StatusCode != http.StatusOK || payload["id"] == nil {
				t.Fatalf("%s failure did not fall back: status=%d payload=%v", tc.name, resp.StatusCode, payload)
			}
		})
	}
}

func TestOrderedFallbackStopsAfterFirstSuccess(t *testing.T) {
	var secondCalls atomic.Int32
	first := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok", "object": "chat.completion", "model": "model-a", "choices": []any{}})
	})
	second := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		secondCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "unexpected", "object": "chat.completion", "model": "model-b"})
	})
	api, secret, canonical := notificationTestHarness(t, first, second)
	resp, payload := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != http.StatusOK || secondCalls.Load() != 0 {
		t.Fatalf("first-target success reached fallback: status=%d second_calls=%d payload=%v", resp.StatusCode, secondCalls.Load(), payload)
	}
}

func TestDirectRealRequestDoesNotInheritVirtualFallback(t *testing.T) {
	var fallbackCalls atomic.Int32
	failing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	second := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		fallbackCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	api, _, _ := notificationTestHarness(t, failing, second)
	status, providersPayload, _ := api.request(http.MethodGet, "/api/admin/providers", nil)
	if status != http.StatusOK {
		t.Fatalf("list providers: %d %v", status, providersPayload)
	}
	var providerID string
	for _, raw := range providersPayload["data"].([]any) {
		provider := raw.(map[string]any)
		if provider["name"] == "provider-a" {
			providerID = provider["id"].(string)
		}
	}
	status, modelsPayload, _ := api.request(http.MethodGet, "/api/admin/providers/"+providerID+"/models", nil)
	if status != http.StatusOK {
		t.Fatalf("list models: %d %v", status, modelsPayload)
	}
	var modelID string
	for _, raw := range modelsPayload["data"].([]any) {
		model := raw.(map[string]any)
		if model["upstream_model_id"] == "model-a" {
			modelID = model["id"].(string)
		}
	}
	status, keyPayload, _ := api.request(http.MethodPost, "/api/admin/client-keys", map[string]any{"name": "direct client", "type": "catalogue"})
	if status != http.StatusCreated {
		t.Fatalf("create direct client: %d %v", status, keyPayload)
	}
	clientID, secret := keyPayload["id"].(string), keyPayload["secret"].(string)
	status, permissionPayload, _ := api.request(http.MethodPut, "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{
		"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}},
	})
	if status != http.StatusNoContent {
		t.Fatalf("set direct permission: %d %v", status, permissionPayload)
	}
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{}})
	if resp.StatusCode != http.StatusInternalServerError || fallbackCalls.Load() != 0 {
		t.Fatalf("direct request used virtual fallback: status=%d fallback_calls=%d", resp.StatusCode, fallbackCalls.Load())
	}
}

func TestClientCancellationDoesNotAttemptNextTarget(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var secondCalls atomic.Int32
	first := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	second := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	api, secret, canonical := notificationTestHarness(t, first, second)
	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{"model": canonical, "messages": []any{}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, api.base+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first target was not reached")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("cancelled client request did not finish")
	}
	close(release)
	if secondCalls.Load() != 0 {
		t.Fatalf("client cancellation attempted %d fallback targets", secondCalls.Load())
	}
}

func TestStreamFailureAfterOutputDoesNotSpliceFallback(t *testing.T) {
	var secondCalls atomic.Int32
	first := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"model\":\"model-a\",\"choices\":[]}\n\n")
		w.(http.Flusher).Flush()
	})
	second := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	api, secret, canonical := notificationTestHarness(t, first, second)
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "stream": true, "messages": []any{}})
	_, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if secondCalls.Load() != 0 {
		t.Fatalf("stream failure spliced %d fallback targets", secondCalls.Load())
	}
}

func TestInFlightRouteKeepsResolvedTargetWhileNewRequestsUseUpdate(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	first := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "from-a", "object": "chat.completion", "model": "model-a", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "from-a"}}}})
	})
	second := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "from-b", "object": "chat.completion", "model": "model-b", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "from-b"}}}})
	})
	api, secret, canonical := notificationTestHarness(t, first, second)
	type callResult struct {
		status  int
		payload map[string]any
	}
	inFlight := make(chan callResult, 1)
	go func() {
		resp, payload := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
		inFlight <- callResult{status: resp.StatusCode, payload: payload}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not reach the original target")
	}

	status, virtualPayload, _ := api.request(http.MethodGet, "/api/admin/virtual-models", nil)
	if status != http.StatusOK {
		t.Fatalf("list virtual models: %d %v", status, virtualPayload)
	}
	virtual := virtualPayload["data"].([]any)[0].(map[string]any)
	targets := virtual["targets"].([]any)
	reversed := []any{
		map[string]any{"provider_model_id": targets[1].(map[string]any)["provider_model_id"], "enabled": true},
		map[string]any{"provider_model_id": targets[0].(map[string]any)["provider_model_id"], "enabled": true},
	}
	status, patchPayload, _ := api.request(http.MethodPatch, "/api/admin/virtual-models/"+virtual["id"].(string), map[string]any{"routing_mode": "ordered_fallback", "targets": reversed})
	if status != http.StatusNoContent {
		t.Fatalf("reorder virtual targets: %d %v", status, patchPayload)
	}
	close(release)
	result := <-inFlight
	if result.status != http.StatusOK || result.payload["id"] != "from-a" {
		t.Fatalf("in-flight request changed target: status=%d payload=%v", result.status, result.payload)
	}
	resp, payload := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{}})
	if resp.StatusCode != http.StatusOK || payload["id"] != "from-b" {
		t.Fatalf("new request did not use reordered target: status=%d payload=%v", resp.StatusCode, payload)
	}
}
