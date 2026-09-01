package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

// notificationTestHarness wires up a router, an admin session, a client key,
// and a virtual model with ordered fallback across two providers. The first
// provider's chat endpoint is served by failUpstream (which fails), the second
// by okUpstream (which succeeds). It returns the api, client secret, and the
// virtual model canonical name.
func notificationTestHarness(t *testing.T, failUpstream, okUpstream http.HandlerFunc) (*testAPI, string, string) {
	t.Helper()
	failServer := httptest.NewServer(failUpstream)
	t.Cleanup(failServer.Close)
	okServer := httptest.NewServer(okUpstream)
	t.Cleanup(okServer.Close)

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := New(config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)

	providerIDs := map[string]string{}
	modelIDs := map[string]string{}
	for _, p := range []struct{ name, url string }{{"provider-a", failServer.URL + "/v1"}, {"provider-b", okServer.URL + "/v1"}} {
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
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "notify client"})
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

// failUpstream serves a catalogue but always fails chat completions.
func failUpstream(t *testing.T) http.HandlerFunc {
	return failUpstreamModel(t, "model-a")
}

// failUpstreamModel serves a catalogue exposing the given model id but always
// fails chat completions.
func failUpstreamModel(t *testing.T, modelID string) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": modelID}}})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "upstream failure", 500)
	})
}

// okUpstream serves a catalogue and succeeds on chat completions.
func okUpstream(t *testing.T) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-b"}}})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "response-1", "object": "chat.completion", "model": "model-b", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}})
	})
}

// waitForNotification blocks until a notification arrives or the timeout elapses.
func waitForNotification(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for notification")
		return ""
	}
}

func TestNotificationSettingsRoundTrip(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	status, payload, _ := api.request("GET", "/api/admin/settings", nil)
	if status != 200 {
		t.Fatalf("get settings: %d %v", status, payload)
	}
	if v, ok := payload["notifications_enabled"].(bool); !ok || v {
		t.Fatalf("notifications should default to disabled, got %v", payload["notifications_enabled"])
	}
	if v, ok := payload["notifications_event_fallback"].(bool); !ok || !v {
		t.Fatalf("fallback event should default to enabled, got %v", payload["notifications_event_fallback"])
	}
	if v, ok := payload["notifications_auth_header_set"].(bool); !ok || v {
		t.Fatalf("auth header should default to unset, got %v", payload["notifications_auth_header_set"])
	}

	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{
		"notifications_enabled":          true,
		"notifications_webhook_url":      "https://ntfy.example.com/tiller",
		"notifications_event_fallback":   false,
		"notifications_event_all_failed": true,
		"notifications_auth_header":      "Bearer secret-token",
	})
	if status != 204 {
		t.Fatalf("update settings: %d", status)
	}
	status, payload, _ = api.request("GET", "/api/admin/settings", nil)
	if status != 200 {
		t.Fatalf("get settings after update: %d %v", status, payload)
	}
	if v, ok := payload["notifications_enabled"].(bool); !ok || !v {
		t.Fatalf("notifications_enabled not persisted: %v", payload["notifications_enabled"])
	}
	if v, ok := payload["notifications_webhook_url"].(string); !ok || v != "https://ntfy.example.com/tiller" {
		t.Fatalf("webhook url not persisted: %v", payload["notifications_webhook_url"])
	}
	if v, ok := payload["notifications_event_fallback"].(bool); !ok || v {
		t.Fatalf("fallback toggle not persisted: %v", payload["notifications_event_fallback"])
	}
	if v, ok := payload["notifications_auth_header_set"].(bool); !ok || !v {
		t.Fatalf("auth header set flag not persisted: %v", payload["notifications_auth_header_set"])
	}
	// The secret value itself must never be returned.
	if _, ok := payload["notifications_auth_header"].(string); ok {
		t.Fatalf("auth header secret leaked in settings response: %v", payload)
	}

	// Invalid webhook URL rejected.
	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{"notifications_webhook_url": "not-a-url"})
	if status != 400 {
		t.Fatalf("invalid webhook url should be rejected, got %d", status)
	}

	// Clearing the auth header: an empty (non-nil) value clears it.
	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{"notifications_auth_header": ""})
	if status != 204 {
		t.Fatalf("clear auth header: %d", status)
	}
	status, payload, _ = api.request("GET", "/api/admin/settings", nil)
	if v, ok := payload["notifications_auth_header_set"].(bool); !ok || v {
		t.Fatalf("auth header should be cleared, got %v", payload["notifications_auth_header_set"])
	}

	// Omitting the auth-header field entirely leaves the (still-cleared) value
	// unchanged — nil means "not present", never "clear".
	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{"notifications_webhook_url": "https://ntfy.example.com/tiller"})
	if status != 204 {
		t.Fatalf("settings update without auth field: %d", status)
	}
	status, payload, _ = api.request("GET", "/api/admin/settings", nil)
	if v, ok := payload["notifications_auth_header_set"].(bool); !ok || v {
		t.Fatalf("omitted auth field must not clear the header, got %v", payload["notifications_auth_header_set"])
	}
}

func TestNotificationTestEndpoint(t *testing.T) {
	received := make(chan string, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(200)
	}))
	defer webhook.Close()

	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	// No URL configured -> error.
	status, _, _ := api.request("POST", "/api/admin/notifications/test", nil)
	if status != 400 {
		t.Fatalf("test without url should be 400, got %d", status)
	}
	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{"notifications_webhook_url": webhook.URL, "notifications_auth_header": "Bearer test-token"})
	if status != 204 {
		t.Fatalf("update settings: %d", status)
	}
	status, _, _ = api.request("POST", "/api/admin/notifications/test", nil)
	if status != 200 {
		t.Fatalf("test notification: %d", status)
	}
	p := waitForNotification(t, received)
	if !strings.Contains(p, "Tiller test notification") {
		t.Fatalf("unexpected test message: %q", p)
	}
}

func TestNotificationFallbackEvent(t *testing.T) {
	received := make(chan string, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer webhook-token" {
			t.Errorf("webhook did not receive the configured Authorization header")
		}
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(200)
	}))
	defer webhook.Close()

	api, secret, canonical := notificationTestHarness(t, failUpstream(t), okUpstream(t))
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{
		"notifications_enabled":          true,
		"notifications_webhook_url":      webhook.URL,
		"notifications_event_fallback":   true,
		"notifications_event_all_failed": true,
		"notifications_auth_header":      "Bearer webhook-token",
	})
	if status != 204 {
		t.Fatalf("update settings: %d", status)
	}
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("fallback request should succeed, got %d", resp.StatusCode)
	}
	p := waitForNotification(t, received)
	for _, want := range []string{
		"Tiller fallback",
		"Client: notify client",
		"Failed #1: provider-a/model-a",
		"Succeeded: provider-b/model-b",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("message missing %q: %q", want, p)
		}
	}
}

func TestNotificationAllTargetsFailedEvent(t *testing.T) {
	received := make(chan string, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(200)
	}))
	defer webhook.Close()

	api, secret, canonical := notificationTestHarness(t, failUpstreamModel(t, "model-a"), failUpstreamModel(t, "model-b"))
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{
		"notifications_enabled":          true,
		"notifications_webhook_url":      webhook.URL,
		"notifications_event_fallback":   true,
		"notifications_event_all_failed": true,
	})
	if status != 204 {
		t.Fatalf("update settings: %d", status)
	}
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if resp.StatusCode != 503 {
		t.Fatalf("all-targets-failed request should be 503, got %d", resp.StatusCode)
	}
	p := waitForNotification(t, received)
	for _, want := range []string{
		"Tiller routing failed",
		"Client: notify client",
		"Failed #1: provider-a/model-a",
		"Failed #2: provider-b/model-b",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("message missing %q: %q", want, p)
		}
	}
}

func TestNotificationEventToggleRespected(t *testing.T) {
	received := make(chan string, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(200)
	}))
	defer webhook.Close()

	api, secret, canonical := notificationTestHarness(t, failUpstream(t), okUpstream(t))
	// Fallback event disabled.
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{
		"notifications_enabled":          true,
		"notifications_webhook_url":      webhook.URL,
		"notifications_event_fallback":   false,
		"notifications_event_all_failed": true,
	})
	if status != 204 {
		t.Fatalf("update settings: %d", status)
	}
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("request should succeed, got %d", resp.StatusCode)
	}
	select {
	case p := <-received:
		t.Fatalf("notification should not fire when fallback event disabled, got %q", p)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestAttemptCountExcludesSkippedTargets guards the payload's attempt_count
// semantics: targets skipped without an upstream attempt (unavailable or
// protocol-mismatched) are not "attempted" and must not inflate the count.
func TestAttemptCountExcludesSkippedTargets(t *testing.T) {
	attempts := []requestAttempt{
		{provider: "a", model: "m", result: "skipped", failureClass: "unavailable"},
		{provider: "b", model: "m", result: "failed", failureClass: "http_500"},
		{provider: "c", model: "m", result: "skipped", failureClass: "protocol_unavailable"},
		{provider: "d", model: "m", result: "success"},
	}
	if got := attemptCount(attempts); got != 2 {
		t.Fatalf("attemptCount = %d, want 2 (only real upstream attempts)", got)
	}
	if hasFailedAttempt(attempts) != true {
		t.Fatal("hasFailedAttempt should be true with one failed attempt")
	}
	allSkipped := []requestAttempt{{provider: "a", model: "m", result: "skipped", failureClass: "unavailable"}}
	if hasFailedAttempt(allSkipped) {
		t.Fatal("hasFailedAttempt must be false when targets were only skipped")
	}
}

func TestNotificationFailureDoesNotFailInference(t *testing.T) {
	// Webhook that responds slowly to prove delivery never blocks the response.
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(200)
	}))
	defer webhook.Close()

	api, secret, canonical := notificationTestHarness(t, failUpstream(t), okUpstream(t))
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{
		"notifications_enabled":          true,
		"notifications_webhook_url":      webhook.URL,
		"notifications_event_fallback":   true,
		"notifications_event_all_failed": true,
	})
	if status != 204 {
		t.Fatalf("update settings: %d", status)
	}
	start := time.Now()
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	elapsed := time.Since(start)
	if resp.StatusCode != 200 {
		t.Fatalf("request should succeed despite webhook hang, got %d", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("notification delivery materially delayed the response: %v", elapsed)
	}
}
