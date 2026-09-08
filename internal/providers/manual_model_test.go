package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
)

func manualModelTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertTestProvider(t *testing.T, db *database.DB, id, name, typ, baseURL string) {
	t.Helper()
	now := database.Now()
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES(?,?,?)`, name, "real", id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,credential_secret,enabled,protocols,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, name, typ, baseURL, nil, 1, EncodeProtocols([]Protocol{ProtocolChat}), now, now); err != nil {
		t.Fatal(err)
	}
}

func setModelsDev(t *testing.T, registry *Registry, dataset modelsDevDataset) {
	t.Helper()
	registry.mu.Lock()
	registry.modelsDev = dataset
	registry.modelsDevEnabled = true
	registry.mu.Unlock()
}

func discoveryServer(t *testing.T, models ...map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
	}))
	t.Cleanup(server.Close)
	return server
}

func manualModelRow(t *testing.T, db *database.DB, modelID string) (string, int, sql.NullInt64, sql.NullString) {
	t.Helper()
	var origin string
	var available int
	var contextLength sql.NullInt64
	var protocol sql.NullString
	if err := db.SQL.QueryRow(`SELECT origin,available,context_length,native_protocol FROM provider_models WHERE id=?`, modelID).Scan(&origin, &available, &contextLength, &protocol); err != nil {
		t.Fatal(err)
	}
	return origin, available, contextLength, protocol
}

func TestResolveManualModelPrefersLiveDiscovery(t *testing.T) {
	upstream := discoveryServer(t, map[string]any{
		"id":                   "live-model",
		"context_length":       111,
		"max_output_tokens":    22,
		"supported_parameters": []string{"tools"},
		"architecture":         map[string]any{"input_modalities": []string{"text", "image"}, "output_modalities": []string{"text"}},
	})
	db := manualModelTestDB(t)
	insertTestProvider(t, db, "provider-live", "live", "deepseek", upstream.URL+"/v1")
	registry := NewRegistry()
	setModelsDev(t, registry, modelsDevDataset{"deepseek": {Models: map[string]modelsDevModel{
		"live-model": {Limit: modelsDevLimit{Context: 999, Output: 888}, ToolCall: boolPtr(true), Reasoning: boolPtr(true)},
	}}})
	m := NewManager(db.SQL, registry)

	model, err := m.ResolveManualModel(context.Background(), "provider-live", "live-model")
	if err != nil {
		t.Fatal(err)
	}
	if model.ContextLength != 111 || model.MaxOutputTokens != 22 {
		t.Fatalf("live limits not preferred: context=%d output=%d", model.ContextLength, model.MaxOutputTokens)
	}
	if model.SupportsTools == nil || !*model.SupportsTools {
		t.Fatalf("expected live tool support, got %v", model.SupportsTools)
	}
	if model.SupportsVision == nil || !*model.SupportsVision {
		t.Fatalf("expected live vision support, got %v", model.SupportsVision)
	}
	if model.SupportsReasoning == nil || *model.SupportsReasoning {
		t.Fatalf("live reasoning=false must not be overridden by models.dev, got %v", model.SupportsReasoning)
	}
}

func TestResolveManualModelFallsBackToModelsDev(t *testing.T) {
	upstream := discoveryServer(t, map[string]any{"id": "other-model"})
	db := manualModelTestDB(t)
	insertTestProvider(t, db, "provider-md", "md", "deepseek", upstream.URL+"/v1")
	registry := NewRegistry()
	setModelsDev(t, registry, modelsDevDataset{"deepseek": {Models: map[string]modelsDevModel{
		"detect-me": {
			Limit:            modelsDevLimit{Context: 12345, Output: 678},
			ToolCall:         boolPtr(true),
			Reasoning:        boolPtr(true),
			StructuredOutput: boolPtr(true),
			Modalities:       modelsDevModalities{Input: []string{"text", "image"}, Output: []string{"text"}},
		},
	}}})
	m := NewManager(db.SQL, registry)

	model, err := m.ResolveManualModel(context.Background(), "provider-md", "detect-me")
	if err != nil {
		t.Fatal(err)
	}
	if model.ContextLength != 12345 || model.MaxOutputTokens != 678 {
		t.Fatalf("models.dev limits not applied: context=%d output=%d", model.ContextLength, model.MaxOutputTokens)
	}
	if model.SupportsTools == nil || !*model.SupportsTools {
		t.Fatalf("expected models.dev tool support, got %v", model.SupportsTools)
	}
	if model.SupportsVision == nil || !*model.SupportsVision {
		t.Fatalf("expected models.dev vision support, got %v", model.SupportsVision)
	}
	if model.SupportsReasoning == nil || !*model.SupportsReasoning {
		t.Fatalf("expected models.dev reasoning support, got %v", model.SupportsReasoning)
	}
	if model.SupportsStructuredOutput == nil || !*model.SupportsStructuredOutput {
		t.Fatalf("expected models.dev structured output support, got %v", model.SupportsStructuredOutput)
	}
	if len(model.InputModalities) != 2 {
		t.Fatalf("expected models.dev input modalities, got %v", model.InputModalities)
	}
}

func TestResolveManualModelSurvivesProbeFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	db := manualModelTestDB(t)
	insertTestProvider(t, db, "provider-down", "down", "deepseek", upstream.URL+"/v1")
	registry := NewRegistry()
	setModelsDev(t, registry, modelsDevDataset{"deepseek": {Models: map[string]modelsDevModel{
		"detect-me": {Limit: modelsDevLimit{Context: 4242, Output: 42}},
	}}})
	m := NewManager(db.SQL, registry)

	model, err := m.ResolveManualModel(context.Background(), "provider-down", "detect-me")
	if err != nil {
		t.Fatal(err)
	}
	if model.ContextLength != 4242 || model.MaxOutputTokens != 42 {
		t.Fatalf("probe failure should fall back to models.dev, got context=%d output=%d", model.ContextLength, model.MaxOutputTokens)
	}
}

func TestAddManualModelOverridesAndPersists(t *testing.T) {
	upstream := discoveryServer(t, map[string]any{"id": "override-model", "context_length": 100})
	db := manualModelTestDB(t)
	insertTestProvider(t, db, "provider-add", "add", "deepseek", upstream.URL+"/v1")
	now := database.Now()
	for _, key := range []struct {
		id   string
		seed int
	}{{"ck-default", 1}, {"ck-none", 0}} {
		if _, err := db.SQL.Exec(`INSERT INTO client_keys(id,name,selector,secret_hash,secret_fingerprint,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, key.id, key.id, "sel-"+key.id, "hash", "fp", now, now); err != nil {
			t.Fatal(err)
		}
		if key.seed > 0 {
			if _, err := db.SQL.Exec(`INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at) VALUES(?,?,?,?,?)`, key.id, "real", "provider-add", 1, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	m := NewManager(db.SQL, NewRegistry())
	override := int64(555)
	modelID, err := m.AddManualModel(context.Background(), "provider-add", ManualModelInput{UpstreamModelID: "override-model", ContextLength: &override, NativeProtocol: ProtocolResponses})
	if err != nil {
		t.Fatal(err)
	}
	origin, available, contextLength, protocol := manualModelRow(t, db, modelID)
	if origin != "manual" || available != 1 {
		t.Fatalf("manual row origin=%q available=%d, want manual/1", origin, available)
	}
	if !contextLength.Valid || contextLength.Int64 != 555 {
		t.Fatalf("admin context override not persisted: %+v", contextLength)
	}
	if !protocol.Valid || protocol.String != string(ProtocolResponses) {
		t.Fatalf("admin protocol override not persisted: %+v", protocol)
	}
	for _, key := range []struct {
		id      string
		enabled int
	}{{"ck-default", 1}, {"ck-none", 0}} {
		var enabled int
		if err := db.SQL.QueryRow(`SELECT enabled FROM client_model_permissions WHERE client_key_id=? AND model_kind='real' AND model_id=?`, key.id, modelID).Scan(&enabled); err != nil {
			t.Fatal(err)
		}
		if enabled != key.enabled {
			t.Fatalf("permission for %s enabled=%d, want %d", key.id, enabled, key.enabled)
		}
	}
}

func TestAddManualModelDuplicateReturnsConflict(t *testing.T) {
	upstream := discoveryServer(t, map[string]any{"id": "dupe-model"})
	db := manualModelTestDB(t)
	insertTestProvider(t, db, "provider-dupe", "dupe", "deepseek", upstream.URL+"/v1")
	m := NewManager(db.SQL, NewRegistry())
	ctx := context.Background()
	if _, err := m.AddManualModel(ctx, "provider-dupe", ManualModelInput{UpstreamModelID: "dupe-model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddManualModel(ctx, "provider-dupe", ManualModelInput{UpstreamModelID: "dupe-model"}); !errors.Is(err, ErrManualModelExists) {
		t.Fatalf("second add err=%v, want ErrManualModelExists", err)
	}
}

func TestAddManualModelUnknownProvider(t *testing.T) {
	db := manualModelTestDB(t)
	m := NewManager(db.SQL, NewRegistry())
	if _, err := m.AddManualModel(context.Background(), "missing", ManualModelInput{UpstreamModelID: "x"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown provider err=%v, want sql.ErrNoRows", err)
	}
}

func TestApplyCatalogueRetainsManualModels(t *testing.T) {
	upstream := discoveryServer(t, map[string]any{"id": "discovered-model"})
	db := manualModelTestDB(t)
	insertTestProvider(t, db, "provider-retain", "retain", "deepseek", upstream.URL+"/v1")
	m := NewManager(db.SQL, NewRegistry())
	ctx := context.Background()
	manualID, err := m.AddManualModel(ctx, "provider-retain", ManualModelInput{UpstreamModelID: "manual-model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.applyCatalogue(ctx, "provider-retain", []Model{{ID: "discovered-model"}}); err != nil {
		t.Fatal(err)
	}
	if _, available, _, _ := manualModelRow(t, db, manualID); available != 1 {
		t.Fatalf("manual model retired after a non-empty refresh")
	}
	if err := m.applyCatalogue(ctx, "provider-retain", nil); err != nil {
		t.Fatal(err)
	}
	if _, available, _, _ := manualModelRow(t, db, manualID); available != 1 {
		t.Fatalf("manual model retired after an empty discovery refresh")
	}
}
