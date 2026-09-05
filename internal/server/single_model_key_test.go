package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
)

func singleKeyClientModels(t *testing.T, base, secret string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	return resp.StatusCode, payload
}

func TestSingleModelKeyEndToEnd(t *testing.T) {
	var reached []string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{
				map[string]any{"id": "model-a", "context_length": 128000, "max_output_tokens": 8192, "supported_parameters": []string{"tools", "reasoning"}, "architecture": map[string]any{"input_modalities": []string{"text", "image"}}},
				map[string]any{"id": "model-b", "context_length": 64000, "max_output_tokens": 4096},
			}})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var input map[string]any
		_ = json.NewDecoder(r.Body).Decode(&input)
		model, _ := input["model"].(string)
		reached = append(reached, model)
		if streaming, _ := input["stream"].(bool); streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"id":"chunk","object":"chat.completion.chunk","model":"`+model+`","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "response-1", "object": "chat.completion", "model": model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
		})
	})
	api, db, _, _ := loggingTestHarness(t, upstream)

	var providerID, modelA, modelB string
	if err := db.SQL.QueryRow(`SELECT id FROM providers WHERE name='provider-a'`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelA); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-b'`).Scan(&modelB); err != nil {
		t.Fatal(err)
	}

	status, payload, _ := api.request("POST", "/api/admin/client-keys", map[string]any{
		"name": "single client", "type": "single", "single_target_type": "real", "single_target_id": modelA,
	})
	if status != 201 {
		t.Fatalf("create Single key: %d %v", status, payload)
	}
	clientID, secret := payload["id"].(string), payload["secret"].(string)
	backupPath, err := db.Backup(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	restored, err := database.Open(context.Background(), backupPath)
	if err != nil {
		t.Fatal(err)
	}
	var restoredType, restoredName, restoredTarget string
	if err := restored.SQL.QueryRow(`SELECT c.key_type,b.exposed_model_name,b.real_model_id FROM client_keys c JOIN client_single_bindings b ON b.client_key_id=c.id WHERE c.id=?`, clientID).Scan(&restoredType, &restoredName, &restoredTarget); err != nil {
		restored.Close()
		t.Fatal(err)
	}
	restored.Close()
	if restoredType != "single" || restoredName != "main" || restoredTarget != modelA {
		t.Fatalf("backup lost Single binding: type=%q name=%q target=%q", restoredType, restoredName, restoredTarget)
	}

	status, catalogue := singleKeyClientModels(t, api.base, secret)
	if status != 200 {
		t.Fatalf("Single catalogue: %d %v", status, catalogue)
	}
	models := catalogue["data"].([]any)
	if len(models) != 1 || models[0].(map[string]any)["id"] != "main" || models[0].(map[string]any)["context_length"] != float64(128000) {
		t.Fatalf("Single catalogue leaked or lost metadata: %v", catalogue)
	}
	if models[0].(map[string]any)["supports_tools"] != float64(1) || models[0].(map[string]any)["supports_vision"] != float64(1) {
		t.Fatalf("Single catalogue did not inherit bound-model capabilities: %v", catalogue)
	}

	resp, body := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "hidden/typo", "messages": []any{}})
	if resp.StatusCode != 200 || body["model"] != "main" || len(reached) != 1 || reached[0] != "model-a" {
		t.Fatalf("direct Single route escaped binding: status=%d body=%v reached=%v", resp.StatusCode, body, reached)
	}
	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"messages": []any{}})
	if resp.StatusCode != 400 {
		t.Fatalf("missing model must remain invalid: %d", resp.StatusCode)
	}
	for _, request := range []struct {
		path string
		body map[string]any
	}{
		{path: "/v1/responses", body: map[string]any{"model": "wrong-responses", "input": "hello"}},
		{path: "/v1/messages", body: map[string]any{"model": "wrong-messages", "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}},
	} {
		resp, body = clientCall(t, api.base, secret, request.path, request.body)
		if resp.StatusCode != 200 || body["model"] != "main" || reached[len(reached)-1] != "model-a" {
			t.Fatalf("Single protocol route failed for %s: status=%d body=%v reached=%v", request.path, resp.StatusCode, body, reached)
		}
	}
	streamResp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "wrong-stream", "stream": true, "messages": []any{}})
	streamBody, err := io.ReadAll(streamResp.Body)
	streamResp.Body.Close()
	if err != nil || streamResp.StatusCode != 200 || !strings.Contains(string(streamBody), `"model":"main"`) || !strings.Contains(string(streamBody), "data: [DONE]") {
		t.Fatalf("Single stream identity failed: status=%d body=%s err=%v", streamResp.StatusCode, streamBody, err)
	}

	status, payload, _ = api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("create virtual group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "coding", "target_provider_id": providerID, "target_model_id": modelB})
	if status != 201 {
		t.Fatalf("create virtual model: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)
	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"single_target_type": "virtual", "single_target_id": virtualID})
	if status != 204 {
		t.Fatalf("inline target switch: %d %v", status, payload)
	}
	resp, body = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "anything", "messages": []any{}})
	if resp.StatusCode != 200 || body["model"] != "main" || reached[len(reached)-1] != "model-b" {
		t.Fatalf("virtual Single route failed: status=%d body=%v reached=%v", resp.StatusCode, body, reached)
	}

	status, payload, _ = api.request("DELETE", "/api/admin/virtual-models/"+virtualID, nil)
	if status != 409 || payload["error"].(map[string]any)["code"] != "single_binding_in_use" {
		t.Fatalf("bound virtual deletion not blocked: %d %v", status, payload)
	}
	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"single_model_name": "coding"})
	if status != 409 {
		t.Fatalf("rename should require confirmation: %d %v", status, payload)
	}
	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"single_model_name": "coding", "confirm_model_name_change": true})
	if status != 204 {
		t.Fatalf("confirmed rename: %d %v", status, payload)
	}

	status, payload, _ = api.request("GET", "/api/admin/client-keys/"+clientID+"/activity", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	activity := payload["data"].([]any)[0].(map[string]any)
	if activity["requested_model"] != "anything" || activity["exposed_model"] != "main" || activity["route_kind"] != "virtual" || activity["route_model"] != "virtual/coding" || activity["resolved_model"] != "model-b" {
		t.Fatalf("Single routing metadata incomplete: %v", activity)
	}
	status, payload, _ = api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	if _, ok := payload["virtual_models"].(map[string]any)["virtual/coding"]; !ok {
		t.Fatalf("Single virtual traffic was not attributed to its bound abstraction: %v", payload["virtual_models"])
	}

	if _, err := db.SQL.Exec(`UPDATE provider_models SET available=0 WHERE id=?`, modelB); err != nil {
		t.Fatal(err)
	}
	status, catalogue = singleKeyClientModels(t, api.base, secret)
	if status != 200 || len(catalogue["data"].([]any)) != 1 || catalogue["data"].([]any)[0].(map[string]any)["id"] != "coding" {
		t.Fatalf("broken binding disappeared from catalogue: %d %v", status, catalogue)
	}
	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "coding", "messages": []any{}})
	if resp.StatusCode != 503 {
		t.Fatalf("broken binding should return 503: %d", resp.StatusCode)
	}

	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"type": "catalogue"})
	if status != 204 {
		t.Fatalf("switch to Catalogue: %d %v", status, payload)
	}
	resp, _ = clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{}})
	if resp.StatusCode != 404 {
		t.Fatalf("inactive Single binding affected Catalogue permissions: %d", resp.StatusCode)
	}
	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"type": "single"})
	if status != 204 {
		t.Fatalf("restore Single binding: %d %v", status, payload)
	}

	status, payload, _ = api.request("POST", "/api/admin/client-keys/"+clientID+"/rotate", nil)
	if status != 200 {
		t.Fatalf("rotate Single key: %d %v", status, payload)
	}
	newSecret := payload["secret"].(string)
	oldStatus, _ := singleKeyClientModels(t, api.base, secret)
	newStatus, newCatalogue := singleKeyClientModels(t, api.base, newSecret)
	if oldStatus != 401 || newStatus != 200 || newCatalogue["data"].([]any)[0].(map[string]any)["id"] != "coding" {
		t.Fatalf("rotation did not preserve binding: old=%d new=%d catalogue=%v", oldStatus, newStatus, newCatalogue)
	}

	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"single_target_type": "real", "single_target_id": modelA})
	if status != 204 {
		t.Fatalf("repoint to direct model: %d %v", status, payload)
	}
	if _, err := db.SQL.Exec(`UPDATE provider_models SET available=0 WHERE id=?`, modelA); err != nil {
		t.Fatal(err)
	}
	status, catalogue = singleKeyClientModels(t, api.base, newSecret)
	if status != 200 || catalogue["data"].([]any)[0].(map[string]any)["id"] != "coding" {
		t.Fatalf("broken direct binding disappeared from catalogue: %d %v", status, catalogue)
	}
	resp, _ = clientCall(t, api.base, newSecret, "/v1/chat/completions", map[string]any{"model": "anything", "messages": []any{}})
	if resp.StatusCode != 503 {
		t.Fatalf("broken direct binding should return 503: %d", resp.StatusCode)
	}
	status, payload, _ = api.request("DELETE", "/api/admin/providers/"+providerID, nil)
	if status != 409 || payload["error"].(map[string]any)["code"] != "single_binding_in_use" {
		t.Fatalf("bound provider deletion not blocked: %d %v", status, payload)
	}
}

// TestRepointedSingleKeyDoesNotAttributeRealTrafficToOldVirtualModel verifies
// that a real request made after a Single key is repointed away from a virtual
// model is not pulled into the old virtual model's Activity view merely because
// the client still sends the old model identifier.
func TestRepointedSingleKeyDoesNotAttributeRealTrafficToOldVirtualModel(t *testing.T) {
	api, db, _, _ := loggingTestHarness(t, mockUpstream(t))

	var providerID, modelID string
	if err := db.SQL.QueryRow(`SELECT id FROM providers WHERE name='provider-a'`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}

	status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("create virtual group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{
		"group_id": groupID, "name": "coding", "target_provider_id": providerID, "target_model_id": modelID,
	})
	if status != 201 {
		t.Fatalf("create virtual model: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)

	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{
		"name": "repointed single", "type": "single", "single_target_type": "virtual", "single_target_id": virtualID,
	})
	if status != 201 {
		t.Fatalf("create Single key: %d %v", status, payload)
	}
	clientID, clientSecret := payload["id"].(string), payload["secret"].(string)

	// This row is valid historical virtual traffic and must remain visible.
	resp, _ := clientCall(t, api.base, clientSecret, "/v1/chat/completions", map[string]any{
		"model": "anything", "messages": []any{},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("virtual request status: %d", resp.StatusCode)
	}

	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{
		"single_target_type": "real", "single_target_id": modelID,
	})
	if status != 204 {
		t.Fatalf("repoint to real model: %d %v", status, payload)
	}

	var bindingReal, bindingVirtual string
	if err := db.SQL.QueryRow(`SELECT coalesce(real_model_id,''),coalesce(virtual_model_id,'') FROM client_single_bindings WHERE client_key_id=?`, clientID).Scan(&bindingReal, &bindingVirtual); err != nil {
		t.Fatal(err)
	}
	if bindingReal != modelID || bindingVirtual != "" {
		t.Fatalf("repoint left stale binding: real=%q virtual=%q", bindingReal, bindingVirtual)
	}

	// The old client-facing identifier is intentionally sent again. Single
	// routing ignores it and must log the actual real route.
	resp, _ = clientCall(t, api.base, clientSecret, "/v1/chat/completions", map[string]any{
		"model": "virtual/coding", "messages": []any{},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("repointed real request status: %d", resp.StatusCode)
	}

	status, payload, _ = api.request("GET", "/api/admin/virtual-models/"+virtualID+"/activity", nil)
	if status != 200 {
		t.Fatalf("virtual activity: %d %v", status, payload)
	}
	rows := payload["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected only historical virtual traffic, got %d rows: %v", len(rows), rows)
	}
	row := rows[0].(map[string]any)
	if row["route_kind"] != "virtual" || row["route_model_id"] != virtualID {
		t.Fatalf("virtual activity included non-virtual route: %v", row)
	}
}

// TestCatalogueKeyWithStaleBindingDoesNotBlockVirtualOrProviderDelete pins
// V1 §28.10 ("Bound targets cannot be deleted until affected Single keys
// are repointed"). Switching a key from Single to Catalogue leaves a
// client_single_bindings row behind; that row must not block the delete of
// the bound virtual model or the underlying provider, because the key is
// no longer a Single key. Only client_keys.key_type='single' rows count.
func TestCatalogueKeyWithStaleBindingDoesNotBlockVirtualOrProviderDelete(t *testing.T) {
	api, db, _, _ := loggingTestHarness(t, mockUpstream(t))

	var providerID, modelA string
	if err := db.SQL.QueryRow(`SELECT id FROM providers WHERE name='provider-a'`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelA); err != nil {
		t.Fatal(err)
	}

	status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("create virtual group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)

	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{
		"group_id": groupID, "name": "stale-bind-target",
		"target_provider_id": providerID, "target_model_id": modelA,
	})
	if status != 201 {
		t.Fatalf("create virtual model: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)

	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{
		"name": "stale-bind-key", "type": "single",
		"single_target_type": "virtual", "single_target_id": virtualID,
	})
	if status != 201 {
		t.Fatalf("create Single key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)

	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"type": "catalogue"})
	if status != 204 {
		t.Fatalf("switch to Catalogue: %d %v", status, payload)
	}

	var bindingRows int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM client_single_bindings WHERE client_key_id=?`, clientID).Scan(&bindingRows); err != nil {
		t.Fatal(err)
	}
	if bindingRows != 1 {
		t.Fatalf("expected 1 stale binding row after type switch, got %d", bindingRows)
	}

	status, payload, _ = api.request("DELETE", "/api/admin/virtual-models/"+virtualID, nil)
	if status != 204 {
		t.Fatalf("virtual model delete blocked by catalogue-key stale binding: %d %v", status, payload)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM virtual_models WHERE id=?`, virtualID).Scan(&bindingRows); err != nil {
		t.Fatal(err)
	}
	if bindingRows != 0 {
		t.Fatalf("virtual model row still present after 204: %d", bindingRows)
	}

	status, payload, _ = api.request("DELETE", "/api/admin/providers/"+providerID, nil)
	if status != 204 {
		t.Fatalf("provider delete blocked by catalogue-key stale binding: %d %v", status, payload)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM providers WHERE id=?`, providerID).Scan(&bindingRows); err != nil {
		t.Fatal(err)
	}
	if bindingRows != 0 {
		t.Fatalf("provider row still present after 204: %d", bindingRows)
	}
}
