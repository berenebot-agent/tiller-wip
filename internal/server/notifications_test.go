package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "notify client", "type": "catalogue"})
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

// receivedNotification captures the X-Title header and body of a delivered
// webhook notification.
type receivedNotification struct {
	title string
	body  string
}

// waitForNotification blocks until a notification arrives or the timeout elapses.
func waitForNotification(t *testing.T, ch <-chan receivedNotification) receivedNotification {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for notification")
		return receivedNotification{}
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
	if v, ok := payload["notifications_cooldown_seconds"].(float64); !ok || v != 60 {
		t.Fatalf("cooldown should default to 60, got %v", payload["notifications_cooldown_seconds"])
	}
	if v, ok := payload["notifications_event_client_key_created"].(bool); !ok || v {
		t.Fatalf("client-key-created should default to disabled, got %v", payload["notifications_event_client_key_created"])
	}
	if v, ok := payload["notifications_event_client_key_deleted"].(bool); !ok || v {
		t.Fatalf("client-key-deleted should default to disabled, got %v", payload["notifications_event_client_key_deleted"])
	}
	if v, ok := payload["notifications_event_admin_login"].(bool); !ok || !v {
		t.Fatalf("admin-login should default to enabled, got %v", payload["notifications_event_admin_login"])
	}

	status, _, _ = api.request("PUT", "/api/admin/settings", map[string]any{
		"notifications_enabled":                  true,
		"notifications_webhook_url":              "https://ntfy.example.com/tiller",
		"notifications_event_fallback":           false,
		"notifications_event_all_failed":         true,
		"notifications_cooldown_seconds":         120,
		"notifications_event_client_key_created": true,
		"notifications_event_client_key_deleted": true,
		"notifications_event_admin_login":        false,
		"notifications_auth_header":              "Bearer secret-token",
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
	if v, ok := payload["notifications_cooldown_seconds"].(float64); !ok || v != 120 {
		t.Fatalf("cooldown not persisted: %v", payload["notifications_cooldown_seconds"])
	}
	if v, ok := payload["notifications_event_client_key_created"].(bool); !ok || !v {
		t.Fatalf("client-key-created not persisted: %v", payload["notifications_event_client_key_created"])
	}
	if v, ok := payload["notifications_event_client_key_deleted"].(bool); !ok || !v {
		t.Fatalf("client-key-deleted not persisted: %v", payload["notifications_event_client_key_deleted"])
	}
	if v, ok := payload["notifications_event_admin_login"].(bool); !ok || v {
		t.Fatalf("admin-login not persisted: %v", payload["notifications_event_admin_login"])
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
	received := make(chan receivedNotification, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- receivedNotification{title: r.Header.Get("X-Title"), body: string(body)}
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
	n := waitForNotification(t, received)
	if n.title != "Tiller Test Notification" {
		t.Fatalf("unexpected title: %q", n.title)
	}
	if n.body == "" {
		t.Fatal("test notification body should not be empty")
	}
}

func TestNotificationTestEndpointSanitisesTransportErrors(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	const secret = "must-not-appear"
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{"notifications_webhook_url": "http://127.0.0.1:1/notify?token=" + secret})
	if status != http.StatusNoContent {
		t.Fatalf("update settings: %d", status)
	}
	status, payload, _ := api.request("POST", "/api/admin/notifications/test", nil)
	if status != http.StatusBadGateway {
		t.Fatalf("test notification status = %d, want 502", status)
	}
	encoded, _ := json.Marshal(payload)
	if bytes.Contains(encoded, []byte(secret)) || bytes.Contains(encoded, []byte("127.0.0.1")) {
		t.Fatalf("transport error exposed webhook request details: %s", encoded)
	}
}

func TestNotificationFallbackEvent(t *testing.T) {
	received := make(chan receivedNotification, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer webhook-token" {
			t.Errorf("webhook did not receive the configured Authorization header")
		}
		body, _ := io.ReadAll(r.Body)
		received <- receivedNotification{title: r.Header.Get("X-Title"), body: string(body)}
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
	n := waitForNotification(t, received)
	if n.title != "Tiller Fallback - "+canonical {
		t.Fatalf("unexpected title: %q", n.title)
	}
	for _, want := range []string{
		"Client: notify client",
		"Requested model: " + canonical,
		"Failed #1: provider-a/model-a",
		"Succeeded: provider-b/model-b",
	} {
		if !strings.Contains(n.body, want) {
			t.Fatalf("message missing %q: %q", want, n.body)
		}
	}
}

func TestNotificationAllTargetsFailedEvent(t *testing.T) {
	received := make(chan receivedNotification, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- receivedNotification{title: r.Header.Get("X-Title"), body: string(body)}
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
	n := waitForNotification(t, received)
	if n.title != "Tiller Routing Failed - "+canonical {
		t.Fatalf("unexpected title: %q", n.title)
	}
	for _, want := range []string{
		"Client: notify client",
		"Requested model: " + canonical,
		"Failed #1: provider-a/model-a",
		"Failed #2: provider-b/model-b",
	} {
		if !strings.Contains(n.body, want) {
			t.Fatalf("message missing %q: %q", want, n.body)
		}
	}
	if strings.Contains(n.body, "Succeeded:") {
		t.Fatalf("all-targets-failed message must not contain a Succeeded line: %q", n.body)
	}
}

func TestNotificationEventToggleRespected(t *testing.T) {
	received := make(chan receivedNotification, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- receivedNotification{title: r.Header.Get("X-Title"), body: string(body)}
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
		t.Fatalf("notification should not fire when fallback event disabled, got %+v", p)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestNotificationCooldownSuppressesDuplicates(t *testing.T) {
	received := make(chan receivedNotification, 2)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- receivedNotification{title: r.Header.Get("X-Title"), body: string(body)}
		w.WriteHeader(200)
	}))
	defer webhook.Close()

	api, secret, canonical := notificationTestHarness(t, failUpstream(t), okUpstream(t))
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{
		"notifications_enabled":          true,
		"notifications_webhook_url":      webhook.URL,
		"notifications_event_fallback":   true,
		"notifications_event_all_failed": true,
		"notifications_cooldown_seconds": 60,
	})
	if status != 204 {
		t.Fatalf("update settings: %d", status)
	}
	// First fallback for the model -> notification delivered.
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("request should succeed, got %d", resp.StatusCode)
	}
	waitForNotification(t, received)
	// Second fallback for the same model within the cooldown -> suppressed.
	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": canonical, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("request should succeed, got %d", resp.StatusCode)
	}
	select {
	case p := <-received:
		t.Fatalf("duplicate notification should be suppressed by cooldown, got %+v", p)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestAdminEventNotifications(t *testing.T) {
	received := make(chan receivedNotification, 4)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- receivedNotification{title: r.Header.Get("X-Title"), body: string(body)}
		w.WriteHeader(200)
	}))
	defer webhook.Close()

	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{
		"notifications_enabled":                  true,
		"notifications_webhook_url":              webhook.URL,
		"notifications_event_client_key_created": true,
		"notifications_event_client_key_deleted": true,
		"notifications_event_admin_login":        true,
	})
	if status != 204 {
		t.Fatalf("update settings: %d", status)
	}

	// Create a client key -> notification.
	status, payload, _ := api.request("POST", "/api/admin/client-keys", map[string]any{"name": "alert-key", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	keyID := payload["id"].(string)
	n := waitForNotification(t, received)
	if n.title != "Tiller Client key created" {
		t.Fatalf("unexpected title: %q", n.title)
	}
	if !strings.Contains(n.body, "Client: alert-key") {
		t.Fatalf("message missing client name: %q", n.body)
	}

	// Delete the client key -> notification.
	status, _, _ = api.request("DELETE", "/api/admin/client-keys/"+keyID, nil)
	if status != 204 {
		t.Fatalf("delete key: %d", status)
	}
	n = waitForNotification(t, received)
	if n.title != "Tiller Client key deleted" {
		t.Fatalf("unexpected title: %q", n.title)
	}
	if !strings.Contains(n.body, "Client: alert-key") {
		t.Fatalf("message missing client name: %q", n.body)
	}

	// Admin login -> notification.
	status, _, _ = api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d", status)
	}
	n = waitForNotification(t, received)
	if n.title != "Tiller Admin login" {
		t.Fatalf("unexpected title: %q", n.title)
	}
	if !strings.Contains(n.body, "User: admin") {
		t.Fatalf("message missing user: %q", n.body)
	}
}

func TestAdminEventToggleRespected(t *testing.T) {
	received := make(chan receivedNotification, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- receivedNotification{title: r.Header.Get("X-Title"), body: string(body)}
		w.WriteHeader(200)
	}))
	defer webhook.Close()

	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	// Notifications enabled, but the client-key-created toggle is off.
	status, _, _ := api.request("PUT", "/api/admin/settings", map[string]any{
		"notifications_enabled":     true,
		"notifications_webhook_url": webhook.URL,
	})
	if status != 204 {
		t.Fatalf("update settings: %d", status)
	}
	status, payload, _ := api.request("POST", "/api/admin/client-keys", map[string]any{"name": "silent-key", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	select {
	case p := <-received:
		t.Fatalf("notification should not fire when toggle disabled, got %+v", p)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestSendExampleNotificationsToNtfy sends one example of every notification
// type to the real ntfy topic. It is opt-in (TILLER_NOTIFY_EXAMPLES=1) because
// it hits the real network.
func TestSendExampleNotificationsToNtfy(t *testing.T) {
	if os.Getenv("TILLER_NOTIFY_EXAMPLES") == "" {
		t.Skip("set TILLER_NOTIFY_EXAMPLES=1 to send example notifications to the real ntfy topic")
	}
	topic := "https://ntfy.sh/tiller_test_8913ubc081"
	examples := []notificationPayload{
		{Event: eventFallback, Severity: severityWarning, Timestamp: database.Now(), ClientKey: "Agentbox Hermes Argus", RequestedModel: "main/argus", VirtualModel: "main/argus", Attempts: []notificationAttempt{
			{Provider: "nvidia", Model: "moonshotai/kimi-k3", Result: "failed", FailureClass: "http_500", HTTPStatus: 500, LatencyMs: 1234},
			{Provider: "ollama", Model: "deepseek-v4-flash:0731", Result: "success", LatencyMs: 890},
		}},
		{Event: eventAllFailed, Severity: severityError, Timestamp: database.Now(), ClientKey: "Agentbox Hermes Argus", RequestedModel: "main/argus", VirtualModel: "main/argus", Attempts: []notificationAttempt{
			{Provider: "nvidia", Model: "moonshotai/kimi-k3", Result: "failed", FailureClass: "http_500", HTTPStatus: 500, LatencyMs: 1234},
			{Provider: "ollama", Model: "deepseek-v4-flash:0731", Result: "failed", FailureClass: "upstream_timeout", LatencyMs: 5000},
		}},
		{Event: eventTest, Severity: severityWarning, Timestamp: database.Now()},
		{Event: eventClientKeyCreated, Severity: severityInfo, Timestamp: database.Now(), Message: "Client: demo-key\nType: catalogue"},
		{Event: eventClientKeyDeleted, Severity: severityInfo, Timestamp: database.Now(), Message: "Client: demo-key"},
		{Event: eventAdminLogin, Severity: severityInfo, Timestamp: database.Now(), Message: "User: admin\nIP: 192.0.2.1"},
	}
	for _, p := range examples {
		body := []byte(p.humanMessage())
		req, err := http.NewRequest(http.MethodPost, topic, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		req.Header.Set("X-Title", p.heading())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("delivery failed for %s: %v", p.Event, err)
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			t.Fatalf("delivery failed for %s: HTTP %d", p.Event, resp.StatusCode)
		}
	}
}

// TestAttemptCountExcludesSkippedTargets guards the payload's attempt_count
// semantics: targets skipped without an upstream attempt (unavailable or
// protocol-mismatched) are not "attempted" and must not inflate the count.
func TestAttemptCountExcludesSkippedTargets(t *testing.T) {
	attempts := []requestAttempt{
		{providerModelID: "pm-a", provider: "a", model: "m", result: "skipped", failureClass: "unavailable"},
		{providerModelID: "pm-b", provider: "b", model: "m", result: "failed", failureClass: "http_500"},
		{providerModelID: "pm-c", provider: "c", model: "m", result: "skipped", failureClass: "protocol_unavailable"},
		{providerModelID: "pm-d", provider: "d", model: "m", result: "success"},
	}
	if got := attemptCount(attempts); got != 2 {
		t.Fatalf("attemptCount = %d, want 2 (only real upstream attempts)", got)
	}
	if hasFailedAttempt(attempts) != true {
		t.Fatal("hasFailedAttempt should be true with one failed attempt")
	}
	allSkipped := []requestAttempt{{providerModelID: "pm-a", provider: "a", model: "m", result: "skipped", failureClass: "unavailable"}}
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

func notificationDeliveryHarness(t *testing.T, handler http.Handler) (*Server, *database.DB, *httptest.Server) {
	t.Helper()
	webhook := httptest.NewServer(handler)
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		webhook.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := New(config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		webhook.Close()
		t.Fatal(err)
	}
	t.Cleanup(webhook.Close)
	for key, value := range map[string]string{
		database.SettingNotificationsEnabled:         "true",
		database.SettingNotificationsWebhookURL:      webhook.URL,
		database.SettingNotificationsEventFallback:   "true",
		database.SettingNotificationsCooldownSeconds: "0",
	} {
		if err := db.SetSetting(context.Background(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	return app, db, webhook
}

func TestNotificationHTTPStatusControlsDelivery(t *testing.T) {
	var status atomic.Int32
	var requests atomic.Int32
	app, _, _ := notificationDeliveryHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	payload := notificationPayload{Event: eventFallback, VirtualModel: "virtual/coding", RequestedModel: "virtual/coding"}
	key := eventFallback + "|" + payload.VirtualModel
	for _, code := range []int{200, 204, 401, 429, 500} {
		status.Store(int32(code))
		app.notifyCooldownMu.Lock()
		delete(app.notifyLastSent, key)
		app.notifyCooldownMu.Unlock()
		app.deliverNotification(eventFallback, payload)
		app.notifyCooldownMu.Lock()
		_, recorded := app.notifyLastSent[key]
		app.notifyCooldownMu.Unlock()
		if recorded != (code >= 200 && code < 300) {
			t.Fatalf("HTTP %d recorded cooldown = %v, want %v", code, recorded, code >= 200 && code < 300)
		}
	}
	if got := requests.Load(); got != 5 {
		t.Fatalf("requests = %d, want one attempt for each status", got)
	}
}

func TestNotificationFailureClearsReservationAndCooldown(t *testing.T) {
	var status atomic.Int32
	var requests atomic.Int32
	app, db, _ := notificationDeliveryHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	if err := db.SetSetting(context.Background(), database.SettingNotificationsCooldownSeconds, "60"); err != nil {
		t.Fatal(err)
	}
	payload := notificationPayload{Event: eventFallback, VirtualModel: "virtual/coding", RequestedModel: "virtual/coding"}
	key := eventFallback + "|" + payload.VirtualModel
	status.Store(500)
	app.deliverNotification(eventFallback, payload)
	app.notifyCooldownMu.Lock()
	_, sentAfterFailure := app.notifyLastSent[key]
	_, reservedAfterFailure := app.notifyInFlight[key]
	app.notifyCooldownMu.Unlock()
	if sentAfterFailure || reservedAfterFailure {
		t.Fatalf("failed delivery left state: sent=%v reserved=%v", sentAfterFailure, reservedAfterFailure)
	}
	status.Store(200)
	app.deliverNotification(eventFallback, payload)
	app.deliverNotification(eventFallback, payload)
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want failed delivery retried once then cooldown suppression", got)
	}
	app.notifyCooldownMu.Lock()
	_, sentAfterSuccess := app.notifyLastSent[key]
	app.notifyCooldownMu.Unlock()
	if !sentAfterSuccess {
		t.Fatal("successful delivery did not record cooldown")
	}
}

func TestNotificationConcurrentFallbacksReserveOneDelivery(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var requests atomic.Int32
	app, _, _ := notificationDeliveryHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	payload := notificationPayload{Event: eventFallback, VirtualModel: "virtual/coding", RequestedModel: "virtual/coding"}
	firstDone := make(chan struct{})
	go func() { app.deliverNotification(eventFallback, payload); close(firstDone) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first notification did not start")
	}
	secondDone := make(chan struct{})
	go func() { app.deliverNotification(eventFallback, payload); close(secondDone) }()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent notification did not observe the in-flight reservation")
	}
	close(release)
	<-firstDone
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want one concurrent delivery", got)
	}
}

func TestNotificationTimeoutClearsReservation(t *testing.T) {
	var phase atomic.Int32
	var requests atomic.Int32
	release := make(chan struct{})
	app, db, _ := notificationDeliveryHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if phase.Load() == 0 {
			<-release
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	app.notifyClient = &http.Client{Timeout: 20 * time.Millisecond}
	if err := db.SetSetting(context.Background(), database.SettingNotificationsCooldownSeconds, "60"); err != nil {
		t.Fatal(err)
	}
	payload := notificationPayload{Event: eventFallback, VirtualModel: "virtual/coding", RequestedModel: "virtual/coding"}
	key := eventFallback + "|" + payload.VirtualModel
	app.deliverNotification(eventFallback, payload)
	app.notifyCooldownMu.Lock()
	_, sentAfterTimeout := app.notifyLastSent[key]
	_, reservedAfterTimeout := app.notifyInFlight[key]
	app.notifyCooldownMu.Unlock()
	if sentAfterTimeout || reservedAfterTimeout {
		t.Fatalf("timed-out delivery left state: sent=%v reserved=%v", sentAfterTimeout, reservedAfterTimeout)
	}
	phase.Store(1)
	close(release)
	app.deliverNotification(eventFallback, payload)
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want timeout retry", got)
	}
}
