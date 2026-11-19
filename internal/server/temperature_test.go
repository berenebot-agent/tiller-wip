package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database/dbtest"
)

func TestTemperatureRouting(t *testing.T) {
	t.Parallel()
	requests := make(chan map[string]json.RawMessage, 16)
	var failFirst atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"no-sampling"},{"id":"sampling"},{"id":"unknown"}]}`))
			return
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		requests <- body
		if failFirst.Load() && string(body["model"]) == `"no-sampling"` {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"message":"synthetic failure"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_mock","type":"message","role":"assistant","model":"synthetic","content":[{"type":"text","text":"A short title"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":3}}`))
	}))
	t.Cleanup(upstream.Close)
	dataDir := t.TempDir()
	// Load a local fixture through the same cache parser used at boot.
	if err := os.WriteFile(filepath.Join(dataDir, "models-dev.json"), []byte(`{"anthropic":{"models":{"no-sampling":{"temperature":false},"sampling":{"temperature":true},"unknown":{}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := dbtest.Open(context.Background(), filepath.Join(dataDir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	app := newTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: dataDir, ListenAddr: ":8080", ModelsDevEnabled: true}, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login status = %d", status)
	}
	api.csrf = payload["csrf_token"].(string)
	status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": "sampling-provider", "type": "anthropic", "base_url": upstream.URL + "/v1", "credential": "synthetic-secret"})
	if status != 201 {
		t.Fatalf("provider status = %d", status)
	}
	providerID := payload["id"].(string)
	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatalf("models status = %d", status)
	}
	ids := map[string]string{}
	permissions := []any{}
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		ids[m["upstream_model_id"].(string)] = m["id"].(string)
		permissions = append(permissions, map[string]any{"kind": "real", "model_id": m["id"], "enabled": true})
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "title client", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("key status = %d", status)
	}
	secret, clientID := payload["secret"].(string), payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("group status = %d", status)
	}
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": payload["id"], "name": "titles", "routing_mode": "ordered_fallback", "targets": []any{
		map[string]any{"provider_model_id": ids["no-sampling"], "enabled": true},
		map[string]any{"provider_model_id": ids["sampling"], "enabled": true},
	}})
	if status != 201 {
		t.Fatalf("virtual status = %d", status)
	}
	permissions = append(permissions, map[string]any{"kind": "virtual", "model_id": payload["id"], "enabled": true})
	status, _, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": permissions})
	if status != 204 {
		t.Fatalf("permissions status = %d", status)
	}
	assertRequest := func(model string, wantTemperature bool) {
		t.Helper()
		select {
		case got := <-requests:
			if string(got["model"]) != `"`+model+`"` {
				t.Errorf("wrong upstream model: %s", got["model"])
			}
			if _, present := got["temperature"]; present != wantTemperature {
				t.Errorf("%s temperature present = %v, want %v", model, present, wantTemperature)
			}
			if wantTemperature && string(got["temperature"]) != "0.2" {
				t.Errorf("temperature changed: %s", got["temperature"])
			}
			if string(got["top_p"]) != "0.8" {
				t.Error("unrelated sampling parameter changed")
			}
		default:
			t.Fatal("no upstream request")
		}
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, model := range []string{"no-sampling", "sampling", "unknown"} {
			t.Run(path+"/"+model, func(t *testing.T) {
				body := map[string]any{"model": "sampling-provider/" + model, "temperature": 0.2, "top_p": 0.8, "stream": false, "max_tokens": 32}
				if path == "/v1/responses" {
					body["input"] = "Write a title"
				} else {
					body["messages"] = []any{map[string]any{"role": "user", "content": "Write a title"}}
				}
				resp, _ := clientCall(t, api.base, secret, path, body)
				if resp.StatusCode != 200 {
					t.Fatalf("request status = %d", resp.StatusCode)
				}
				assertRequest(model, model != "no-sampling")
			})
		}
	}
	failFirst.Store(true)
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "virtual/titles", "temperature": 0.2, "top_p": 0.8, "stream": false, "messages": []any{map[string]any{"role": "user", "content": "Write a title"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("fallback status = %d", resp.StatusCode)
	}
	assertRequest("no-sampling", false)
	assertRequest("sampling", true)
}

func TestOmitTemperaturePreservesOtherFields(t *testing.T) {
	body := []byte(`{"temperature":0,"top_p":0.8,"metadata":{"sequence":9007199254740993},"messages":[{"role":"user","content":"synthetic"}]}`)
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal(body, &before)
	_ = json.Unmarshal(omitTemperature(body), &after)
	delete(before, "temperature")
	if len(before) != len(after) {
		t.Fatal("unexpected field changes")
	}
	for key, value := range before {
		if string(value) != string(after[key]) {
			t.Errorf("field %s changed", key)
		}
	}
	without := []byte(`{ "top_p": 0.8 }`)
	if string(omitTemperature(without)) != string(without) {
		t.Fatal("request without temperature changed")
	}
}
