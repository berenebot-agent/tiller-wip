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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

type testAPI struct {
	t      *testing.T
	base   string
	client *http.Client
	csrf   string
}

func (a *testAPI) request(method, path string, body any) (int, map[string]any, http.Header) {
	a.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, a.base+path, reader)
	if err != nil {
		a.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.csrf != "" && method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", a.csrf)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	payload := map[string]any{}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		_ = json.NewDecoder(resp.Body).Decode(&payload)
	}
	return resp.StatusCode, payload, resp.Header.Clone()
}

func TestV1VirtualRoutingRemapIsolationRotationAndBackup(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	discoveryFails := false
	discoveredModels := []string{"model-a", "model-b"}
	cancelSeen := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			mu.Lock()
			fail := discoveryFails
			models := append([]string(nil), discoveredModels...)
			mu.Unlock()
			if fail {
				http.Error(w, "unavailable", 500)
				return
			}
			data := make([]any, 0, len(models))
			for _, model := range models {
				entry := map[string]any{"id": model}
				switch model {
				case "model-a":
					entry["context_length"] = 128000
					entry["max_output_tokens"] = 16384
				case "model-b":
					entry["context_length"] = 262144
					entry["max_output_tokens"] = 32768
				}
				data = append(data, entry)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Errorf("upstream did not receive only the stored provider credential")
		}
		for _, name := range []string{"Cookie", "OpenAI-Organization", "OpenAI-Project", "x-api-key"} {
			if r.Header.Get(name) != "" {
				t.Errorf("upstream received stripped client header %s", name)
			}
		}
		var input map[string]any
		_ = json.NewDecoder(r.Body).Decode(&input)
		model, _ := input["model"].(string)
		encodedInput, _ := json.Marshal(input)
		mu.Lock()
		reached = append(reached, model)
		mu.Unlock()
		if streaming, _ := input["stream"].(bool); streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			if bytes.Contains(encodedInput, []byte("cancel-me")) {
				_, _ = io.WriteString(w, `data: {"id":"cancel","object":"chat.completion.chunk","model":"`+model+`","choices":[{"index":0,"delta":{"content":"first"},"finish_reason":null}]}`+"\n\n")
				flusher.Flush()
				select {
				case <-r.Context().Done():
					cancelSeen <- struct{}{}
				case <-time.After(2 * time.Second):
				}
				return
			}
			if bytes.Contains(encodedInput, []byte("tool-stream")) {
				first, _ := json.Marshal(map[string]any{"id": "tool", "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{\"q\":`}}}}, "finish_reason": nil}}})
				_, _ = fmt.Fprintf(w, "data: %s\n\n", first)
				flusher.Flush()
				second, _ := json.Marshal(map[string]any{"id": "tool", "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "1}"}}}}, "finish_reason": "tool_calls"}}})
				_, _ = fmt.Fprintf(w, "data: %s\n\n", second)
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			_, _ = io.WriteString(w, `data: {"id":"one","object":"chat.completion.chunk","model":"`+model+`","choices":[{"index":0,"delta":{"content":"first"},"finish_reason":null}]}`+"\n\n")
			flusher.Flush()
			time.Sleep(40 * time.Millisecond)
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "response-1", "object": "chat.completion", "model": model, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}})
	}))
	defer upstream.Close()

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app, err := New(config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(app.Handler())
	defer router.Close()
	jar, _ := cookiejar.New(nil)
	apiClient := &http.Client{Jar: jar}
	api := &testAPI{t: t, base: router.URL, client: apiClient}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": "provider-a", "type": "generic-openai", "base_url": upstream.URL + "/v1", "credential": "provider-secret"})
	if status != 201 {
		t.Fatalf("create provider: %d %v", status, payload)
	}
	providerID := payload["id"].(string)
	status, failedProvider, _ := api.request("POST", "/api/admin/providers", map[string]any{"name": "provider-b", "type": "generic-openai", "base_url": "http://127.0.0.1:1/v1", "credential": "unreachable-secret"})
	if status != 201 || failedProvider["refresh_error"] == "" {
		t.Fatalf("provider must survive failed initial discovery: %d %v", status, failedProvider)
	}
	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	models := payload["data"].([]any)
	modelIDs := map[string]string{}
	for _, raw := range models {
		m := raw.(map[string]any)
		modelIDs[m["upstream_model_id"].(string)] = m["id"].(string)
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "Hermes test", "description": "acceptance", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "Isolated client", "description": "feeder acceptance", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create second key: %d %v", status, payload)
	}
	isolatedClientID := payload["id"].(string)
	isolatedSecret := payload["secret"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "coding", "target_provider_id": providerID, "target_model_id": modelIDs["model-a"]})
	if status != 201 {
		t.Fatalf("virtual: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "virtual", "model_id": virtualID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+isolatedClientID+"/permissions", map[string]any{"defaults": []any{map[string]any{"kind": "real", "group_id": providerID, "enabled": true}}, "permissions": []any{}})
	if status != 204 {
		t.Fatalf("set feeder: %d %v", status, payload)
	}
	mu.Lock()
	discoveredModels = append(discoveredModels, "model-c")
	mu.Unlock()
	status, payload, _ = api.request("POST", "/api/admin/providers/"+providerID+"/refresh", nil)
	if status != 200 {
		t.Fatalf("discover feeder model: %d %v", status, payload)
	}

	clientCall := func(secret, method, path string, body any) (*http.Response, map[string]any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			encoded, _ := json.Marshal(body)
			reader = bytes.NewReader(encoded)
		}
		req, _ := http.NewRequest(method, router.URL+path, reader)
		if path == "/v1/messages" {
			req.Header.Set("x-api-key", secret)
		} else {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Cookie", "client-session=must-not-forward")
		req.Header.Set("OpenAI-Organization", "client-organization")
		req.Header.Set("OpenAI-Project", "client-project")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		decoded := map[string]any{}
		if !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
			_ = json.NewDecoder(resp.Body).Decode(&decoded)
			resp.Body.Close()
		}
		return resp, decoded
	}
	resp, isolatedCatalogue := clientCall(isolatedSecret, "GET", "/v1/models", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("isolated models: %d %v", resp.StatusCode, isolatedCatalogue)
	}
	isolatedData := isolatedCatalogue["data"].([]any)
	if len(isolatedData) != 1 || isolatedData[0].(map[string]any)["id"] != "provider-a/model-c" {
		t.Fatalf("new-model feeder changed existing permissions or missed new model: %v", isolatedData)
	}
	resp, _ = clientCall(isolatedSecret, "POST", "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "guessed"}}})
	if resp.StatusCode != 404 {
		t.Fatalf("second client guessed a disabled existing model: %d", resp.StatusCode)
	}
	resp, catalogue := clientCall(clientSecret, "GET", "/v1/models", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("models: %d %v", resp.StatusCode, catalogue)
	}
	data := catalogue["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "virtual/coding" {
		t.Fatalf("catalogue leaked or omitted models: %v", data)
	}
	if data[0].(map[string]any)["context_length"] != float64(128000) {
		t.Fatalf("catalogue did not surface the target context length: %v", data[0])
	}
	if data[0].(map[string]any)["max_output_tokens"] != float64(16384) {
		t.Fatalf("catalogue did not surface the target output limit: %v", data[0])
	}
	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"enabled": false})
	if status != 204 {
		t.Fatalf("disable client: %d %v", status, payload)
	}
	resp, _ = clientCall(clientSecret, "GET", "/v1/models", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("disabled key remained valid: %d", resp.StatusCode)
	}
	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"enabled": true})
	if status != 204 {
		t.Fatalf("re-enable client: %d %v", status, payload)
	}
	resp, _ = clientCall(clientSecret, "POST", "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hidden"}}})
	if resp.StatusCode != 404 {
		t.Fatalf("guessed hidden model returned %d", resp.StatusCode)
	}
	resp, result := clientCall(clientSecret, "POST", "/v1/chat/completions", map[string]any{"model": "virtual/coding", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if resp.StatusCode != 200 || result["model"] != "virtual/coding" {
		t.Fatalf("virtual response: %d %v", resp.StatusCode, result)
	}
	status, payload, _ = api.request("PATCH", "/api/admin/virtual-models/"+virtualID, map[string]any{"target_provider_id": providerID, "target_model_id": modelIDs["model-b"]})
	if status != 204 {
		t.Fatalf("remap: %d %v", status, payload)
	}
	resp, result = clientCall(clientSecret, "POST", "/v1/chat/completions", map[string]any{"model": "virtual/coding", "messages": []any{map[string]any{"role": "user", "content": "again"}}})
	if resp.StatusCode != 200 || result["model"] != "virtual/coding" {
		t.Fatalf("remap response: %d %v", resp.StatusCode, result)
	}
	mu.Lock()
	if len(reached) < 2 || reached[len(reached)-2] != "model-a" || reached[len(reached)-1] != "model-b" {
		t.Fatalf("immediate remap did not reach A then B: %v", reached)
	}
	mu.Unlock()
	resp, remappedCatalogue := clientCall(clientSecret, "GET", "/v1/models", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("remapped models: %d %v", resp.StatusCode, remappedCatalogue)
	}
	remappedData := remappedCatalogue["data"].([]any)
	if len(remappedData) != 1 || remappedData[0].(map[string]any)["id"] != "virtual/coding" {
		t.Fatalf("remapped catalogue leaked or omitted models: %v", remappedData)
	}
	if remappedData[0].(map[string]any)["context_length"] != float64(262144) {
		t.Fatalf("remap did not propagate the new target context length: %v", remappedData[0])
	}
	if remappedData[0].(map[string]any)["max_output_tokens"] != float64(32768) {
		t.Fatalf("remap did not propagate the new target output limit: %v", remappedData[0])
	}
	resp, _ = clientCall(clientSecret, "POST", "/v1/chat/completions", map[string]any{"model": "virtual/coding", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "stream"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	resp.Body.Close()
	if err != nil || !strings.Contains(line, "virtual/coding") || !strings.Contains(line, "first") {
		t.Fatalf("stream did not preserve virtual identity: %q %v", line, err)
	}
	resp, _ = clientCall(clientSecret, "POST", "/v1/chat/completions", map[string]any{"model": "virtual/coding", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "tool-stream"}}})
	toolStream, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !bytes.Contains(toolStream, []byte(`"id":"call_1"`)) || !bytes.Contains(toolStream, []byte(`"finish_reason":"tool_calls"`)) {
		t.Fatalf("tool stream was not preserved: %s %v", toolStream, err)
	}
	for _, rawLine := range bytes.Split(toolStream, []byte{'\n'}) {
		rawLine = bytes.TrimSpace(rawLine)
		if !bytes.HasPrefix(rawLine, []byte("data:")) {
			continue
		}
		value := bytes.TrimSpace(bytes.TrimPrefix(rawLine, []byte("data:")))
		if bytes.Equal(value, []byte("[DONE]")) {
			continue
		}
		if !json.Valid(value) {
			t.Fatalf("tool stream emitted invalid JSON: %q", value)
		}
	}
	resp, _ = clientCall(clientSecret, "POST", "/v1/responses", map[string]any{"model": "virtual/coding", "stream": true, "input": "protocol-stream"})
	responseEvent, err := bufio.NewReader(resp.Body).ReadString('\n')
	resp.Body.Close()
	if err != nil || responseEvent != "event: response.created\n" {
		t.Fatalf("responses streaming did not start correctly: %q %v", responseEvent, err)
	}
	resp, _ = clientCall(clientSecret, "POST", "/v1/messages", map[string]any{"model": "virtual/coding", "stream": true, "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "protocol-stream"}}})
	messageEvent, err := bufio.NewReader(resp.Body).ReadString('\n')
	resp.Body.Close()
	if err != nil || messageEvent != "event: message_start\n" {
		t.Fatalf("messages streaming did not start correctly: %q %v", messageEvent, err)
	}
	resp, _ = clientCall(clientSecret, "POST", "/v1/chat/completions", map[string]any{"model": "virtual/coding", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "cancel-me"}}})
	_, _ = bufio.NewReader(resp.Body).ReadString('\n')
	resp.Body.Close()
	select {
	case <-cancelSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not cancel the upstream request")
	}
	status, rotated, _ := api.request("POST", "/api/admin/client-keys/"+clientID+"/rotate", nil)
	if status != 200 {
		t.Fatalf("rotate: %d %v", status, rotated)
	}
	newSecret := rotated["secret"].(string)
	resp, _ = clientCall(clientSecret, "GET", "/v1/models", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("old key still valid: %d", resp.StatusCode)
	}
	resp, _ = clientCall(newSecret, "GET", "/v1/models", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("new key invalid: %d", resp.StatusCode)
	}
	backupReq, _ := http.NewRequest(http.MethodGet, router.URL+"/api/admin/backup/export", nil)
	backupResp, err := api.client.Do(backupReq)
	if err != nil {
		t.Fatal(err)
	}
	backupBytes, readErr := io.ReadAll(backupResp.Body)
	backupResp.Body.Close()
	if backupResp.StatusCode != 200 || backupResp.Header.Get("X-Tiller-Secret-Material") != "provider-credentials" || backupResp.Header.Get("Cache-Control") != "no-store" || readErr != nil {
		t.Fatalf("backup contract: %d %v %v", backupResp.StatusCode, backupResp.Header, readErr)
	}
	if bytes.Contains(backupBytes, []byte(newSecret)) {
		t.Fatal("backup contains a plaintext client key")
	}
	restoreDir := t.TempDir()
	restorePath := filepath.Join(restoreDir, "tiller-router.db")
	if err := os.WriteFile(restorePath, backupBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	restoredDB, err := database.Open(context.Background(), restorePath)
	if err != nil {
		t.Fatal(err)
	}
	restoredApp, err := New(config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: restoreDir, ListenAddr: ":8080"}, restoredDB, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		restoredDB.Close()
		t.Fatal(err)
	}
	restoredServer := httptest.NewServer(restoredApp.Handler())
	restoredReq, _ := http.NewRequest(http.MethodGet, restoredServer.URL+"/v1/models", nil)
	restoredReq.Header.Set("Authorization", "Bearer "+newSecret)
	restoredResp, err := http.DefaultClient.Do(restoredReq)
	if err != nil {
		restoredServer.Close()
		restoredDB.Close()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, restoredResp.Body)
	restoredResp.Body.Close()
	if restoredResp.StatusCode != 200 {
		t.Fatalf("restored key did not authenticate: %d", restoredResp.StatusCode)
	}
	for table, minimum := range map[string]int{"providers": 2, "provider_models": 3, "virtual_models": 1, "client_keys": 2, "client_model_permissions": 1} {
		var count int
		if err := restoredDB.SQL.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count < minimum {
			t.Fatalf("backup did not restore %s: count=%d err=%v", table, count, err)
		}
	}
	restoredServer.Close()
	restoredDB.Close()
	mu.Lock()
	discoveryFails = true
	mu.Unlock()
	status, _, _ = api.request("POST", "/api/admin/providers/"+providerID+"/refresh", nil)
	if status != 502 {
		t.Fatalf("failed refresh returned %d", status)
	}
	resp, catalogue = clientCall(newSecret, "GET", "/v1/models", nil)
	if resp.StatusCode != 200 || len(catalogue["data"].([]any)) != 1 {
		t.Fatalf("failed refresh corrupted catalogue: %d %v", resp.StatusCode, catalogue)
	}
	mu.Lock()
	discoveryFails = false
	discoveredModels = []string{"model-a", "model-c"}
	mu.Unlock()
	status, payload, _ = api.request("POST", "/api/admin/providers/"+providerID+"/refresh", nil)
	if status != 200 {
		t.Fatalf("retire target refresh: %d %v", status, payload)
	}
	resp, catalogue = clientCall(newSecret, "GET", "/v1/models", nil)
	if resp.StatusCode != 200 || len(catalogue["data"].([]any)) != 0 {
		t.Fatalf("broken virtual model was not hidden: %d %v", resp.StatusCode, catalogue)
	}
	resp, _ = clientCall(newSecret, "POST", "/v1/chat/completions", map[string]any{"model": "virtual/coding", "messages": []any{map[string]any{"role": "user", "content": "broken"}}})
	if resp.StatusCode != 503 {
		t.Fatalf("broken virtual target returned %d instead of 503", resp.StatusCode)
	}
	status, health, _ := api.request("GET", "/api/admin/health", nil)
	if status != 200 || health["broken_virtual_models"] != float64(1) {
		t.Fatalf("admin health did not warn about broken mapping: %d %v", status, health)
	}
	mu.Lock()
	discoveredModels = []string{"model-a", "model-b", "model-c"}
	mu.Unlock()
	status, payload, _ = api.request("POST", "/api/admin/providers/"+providerID+"/refresh", nil)
	if status != 200 {
		t.Fatalf("restore retired target: %d %v", status, payload)
	}
	resp, catalogue = clientCall(newSecret, "GET", "/v1/models", nil)
	if resp.StatusCode != 200 || len(catalogue["data"].([]any)) != 1 {
		t.Fatalf("restored target missing from catalogue: %d %v", resp.StatusCode, catalogue)
	}
	upstream.CloseClientConnections()
	upstream.Close()
	resp, _ = clientCall(newSecret, "POST", "/v1/chat/completions", map[string]any{"model": "virtual/coding", "messages": []any{map[string]any{"role": "user", "content": "outage"}}})
	if resp.StatusCode != 503 {
		t.Fatalf("exhausted virtual targets returned %d instead of 503", resp.StatusCode)
	}
	resp, catalogue = clientCall(newSecret, "GET", "/v1/models", nil)
	if resp.StatusCode != 200 || len(catalogue["data"].([]any)) != 1 {
		t.Fatalf("provider outage corrupted catalogue: %d %v", resp.StatusCode, catalogue)
	}
}

func TestCatalogueSurfacesCapabilities(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{
			map[string]any{"id": "model-a", "context_length": 128000, "max_output_tokens": 16384, "supported_parameters": []string{"tools", "reasoning", "structured_outputs"}, "architecture": map[string]any{"input_modalities": []string{"text", "image"}, "output_modalities": []string{"text"}}},
			map[string]any{"id": "model-b", "context_length": 262144, "max_output_tokens": 32768},
			map[string]any{"id": "model-c"},
		}})
	}))
	defer upstream.Close()

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app, err := New(config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(app.Handler())
	defer router.Close()
	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": "provider-a", "type": "generic-openai", "base_url": upstream.URL + "/v1", "credential": "provider-secret"})
	if status != 201 {
		t.Fatalf("create provider: %d %v", status, payload)
	}
	providerID := payload["id"].(string)
	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	modelIDs := map[string]string{}
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		modelIDs[m["upstream_model_id"].(string)] = m["id"].(string)
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "cap client", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{
		map[string]any{"kind": "real", "model_id": modelIDs["model-a"], "enabled": true},
		map[string]any{"kind": "real", "model_id": modelIDs["model-b"], "enabled": true},
	}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}
	clientCatalogue := func() map[string]map[string]any {
		t.Helper()
		req, _ := http.NewRequest("GET", router.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+clientSecret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var catalogue map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&catalogue)
		resp.Body.Close()
		byID := map[string]map[string]any{}
		for _, raw := range catalogue["data"].([]any) {
			m := raw.(map[string]any)
			byID[m["id"].(string)] = m
		}
		return byID
	}
	byID := clientCatalogue()
	a := byID["provider-a/model-a"]
	if a["supports_tools"] != float64(1) || a["supports_vision"] != float64(1) || a["supports_reasoning"] != float64(1) || a["supports_structured_output"] != float64(1) {
		t.Fatalf("model-a capabilities not surfaced: %v", a)
	}
	b := byID["provider-a/model-b"]
	for _, key := range []string{"supports_tools", "supports_vision", "supports_reasoning", "supports_structured_output"} {
		if _, ok := b[key]; ok {
			t.Fatalf("model-b unknown capability %s should be omitted: %v", key, b)
		}
	}
	// Virtual effective flags: model-a (tools=1) + model-b (tools=unknown) -> unknown.
	status, payload, _ = api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "mixed", "routing_mode": "ordered_fallback", "targets": []any{
		map[string]any{"provider_model_id": modelIDs["model-a"], "enabled": true},
		map[string]any{"provider_model_id": modelIDs["model-b"], "enabled": true},
	}})
	if status != 201 {
		t.Fatalf("virtual mixed: %d %v", status, payload)
	}
	mixedID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "unknown-limits", "routing_mode": "ordered_fallback", "targets": []any{
		map[string]any{"provider_model_id": modelIDs["model-a"], "enabled": true},
		map[string]any{"provider_model_id": modelIDs["model-c"], "enabled": true},
	}})
	if status != 201 {
		t.Fatalf("virtual unknown limits: %d %v", status, payload)
	}
	unknownLimitsID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "single", "target_provider_id": providerID, "target_model_id": modelIDs["model-a"]})
	if status != 201 {
		t.Fatalf("virtual single: %d %v", status, payload)
	}
	singleID := payload["id"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{
		map[string]any{"kind": "real", "model_id": modelIDs["model-a"], "enabled": true},
		map[string]any{"kind": "real", "model_id": modelIDs["model-b"], "enabled": true},
		map[string]any{"kind": "virtual", "model_id": mixedID, "enabled": true},
		map[string]any{"kind": "virtual", "model_id": unknownLimitsID, "enabled": true},
		map[string]any{"kind": "virtual", "model_id": singleID, "enabled": true},
	}})
	if status != 204 {
		t.Fatalf("permissions2: %d %v", status, payload)
	}
	byID = clientCatalogue()
	mixed := byID["virtual/mixed"]
	if _, ok := mixed["supports_tools"]; ok {
		t.Fatalf("mixed virtual tools should be unknown (model-b unknown), got %v", mixed["supports_tools"])
	}
	unknownLimits := byID["virtual/unknown-limits"]
	if _, ok := unknownLimits["context_length"]; ok {
		t.Fatalf("virtual with an unknown eligible context limit must omit context_length: %v", unknownLimits)
	}
	if _, ok := unknownLimits["max_output_tokens"]; ok {
		t.Fatalf("virtual with an unknown eligible output limit must omit max_output_tokens: %v", unknownLimits)
	}
	single := byID["virtual/single"]
	if single["supports_tools"] != float64(1) || single["supports_vision"] != float64(1) {
		t.Fatalf("single-target virtual should inherit model-a capabilities: %v", single)
	}
}
