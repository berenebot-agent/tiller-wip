package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

func TestHeaderlessSSEIsInspectedByOutputProbe(t *testing.T) {
	const body = "event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n"
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
	}
	defer resp.Body.Close()

	sniffAndClassify(resp)
	if !isStreamingResponse(resp) {
		t.Fatal("headerless SSE was not classified as streaming")
	}
	outcome, err := probeUpstreamOutput(resp, providers.ProtocolResponses)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != probeStreamError {
		t.Fatalf("probe outcome = %v, want stream error", outcome)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("probe changed response body: got %q, want %q", got, body)
	}
}

func TestHeaderlessSSEUsesExistingLineLimit(t *testing.T) {
	body := "event: response.created\ndata: {}\n\n" + "data: " + strings.Repeat("x", maxSSELineBytes) + "\n\n"
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
	}
	defer resp.Body.Close()

	sniffAndClassify(resp)
	if !isStreamingResponse(resp) {
		t.Fatal("headerless SSE was not classified as streaming")
	}
	if err := preflightResponseLimit(resp, maxUpstreamNonStreamBytes); err != nil {
		t.Fatalf("preflight rejected bounded stream: %v", err)
	}
	if err := rewriteSSE(httptest.NewRecorder(), nil, resp.Body, "upstream", "requested", &usageCapture{}); err == nil || !strings.Contains(err.Error(), "SSE line exceeds limit") {
		t.Fatalf("oversized SSE line error = %v, want line-limit failure", err)
	}
}

func TestCodexHeaderlessSSEStreamsIncrementally(t *testing.T) {
	allowComplete := make(chan struct{})
	firstFrameSent := make(chan struct{})
	upstreamDone := make(chan struct{})
	var releaseOnce, firstOnce, doneOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowComplete) }) }
	t.Cleanup(release)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"slug": "gpt-5.6-sol", "display_name": "gpt-5.6-sol", "supported_in_api": true}}})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "gpt-5.6-sol"}}})
		case "/responses":
			_, _ = io.ReadAll(r.Body)
			// An empty value prevents the test server from declaring the usual
			// text/plain content type; the router must classify the body itself.
			w.Header().Set("Content-Type", "")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"first\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			firstOnce.Do(func() { close(firstFrameSent) })
			<-allowComplete
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"model\":\"gpt-5.6-sol\",\"output\":[]}}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			doneOnce.Do(func() { close(upstreamDone) })
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
	var logs bytes.Buffer
	app := newTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	app.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != http.StatusOK {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": "codex-mock", "type": "codex-subscription", "base_url": upstream.URL, "protocols": []any{"responses"}})
	if status != http.StatusCreated {
		t.Fatalf("create provider: %d %v", status, payload)
	}
	providerID := payload["id"].(string)
	future := time.Now().Add(time.Hour)
	putOAuthToken(t, db, oauth.TokenRecord{
		ProviderID:   providerID,
		AccessToken:  "live-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    &future,
		AuthState:    oauth.AuthConnected,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})

	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != http.StatusOK {
		t.Fatal(payload)
	}
	var modelID string
	for _, raw := range payload["data"].([]any) {
		model := raw.(map[string]any)
		if model["upstream_model_id"] == "gpt-5.6-sol" {
			modelID = model["id"].(string)
		}
	}
	if modelID == "" {
		t.Fatal("mock upstream did not expose gpt-5.6-sol")
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "test client", "description": "codex-streaming", "type": "catalogue"})
	if status != http.StatusCreated {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID, clientSecret := payload["id"].(string), payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}}})
	if status != http.StatusNoContent {
		t.Fatalf("permissions: %d %v", status, payload)
	}

	body, _ := json.Marshal(map[string]any{"model": "codex-mock/gpt-5.6-sol", "input": "hello"})
	req, _ := http.NewRequest(http.MethodPost, router.URL+"/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+clientSecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request status = %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("client Content-Type = %q, want SSE", resp.Header.Get("Content-Type"))
	}

	reader := bufio.NewReader(resp.Body)
	var received strings.Builder
	for !strings.Contains(received.String(), "response.output_text.delta") {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("read first SSE event: %v", readErr)
		}
		received.WriteString(line)
	}
	select {
	case <-upstreamDone:
		t.Fatal("upstream completed before the first SSE event reached the client")
	default:
	}
	release()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	received.Write(rest)
	if !strings.Contains(received.String(), "response.completed") {
		t.Fatalf("stream did not complete: %s", received.String())
	}
	if !strings.Contains(logs.String(), "upstream_streaming=true") {
		t.Fatalf("Codex response was not logged as streaming: %s", logs.String())
	}
	select {
	case <-firstFrameSent:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not send the first frame")
	}
}
