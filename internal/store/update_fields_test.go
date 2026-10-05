package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

// patchScope opens a database and returns a scope for the local account plus
// the raw handle the test uses to seed provider fixtures.
func patchScope(t *testing.T) (*store.Scope, *sql.DB) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return store.New(db.SQL).For(database.LocalAccountID), db.SQL
}

func seedPatchClient(t *testing.T, sc *store.Scope, id, name string) {
	t.Helper()
	err := sc.CreateClientKey(context.Background(), store.CreateClientKeyInput{
		ID: id, Name: name, Group: "default", Selector: "sel-" + id, Hash: "hash-" + id,
		Type: "catalogue", LoggingEnabled: true, RetentionDays: 30,
	})
	if err != nil {
		t.Fatalf("seed client key: %v", err)
	}
}

func clientEditable(t *testing.T, sc *store.Scope, id string) store.ClientKeyEditable {
	t.Helper()
	row, err := sc.GetClientKeyEditable(context.Background(), id)
	if err != nil {
		t.Fatalf("read client key: %v", err)
	}
	return row
}

// TestUpdateClientKeyMetadataOnlyPreservesConcurrentFields is the regression for
// the partial-update overwrite: a metadata-only PATCH must not rewrite fields it
// did not supply, so a concurrent change to those fields survives.
func TestUpdateClientKeyMetadataOnlyPreservesConcurrentFields(t *testing.T) {
	sc, _ := patchScope(t)
	ctx := context.Background()
	seedPatchClient(t, sc, "ck-1", "original")

	// Another writer changes fields this PATCH will not mention.
	if err := sc.UpdateClientKey(ctx, store.UpdateClientKeyInput{
		ID: "ck-1", Type: "catalogue", TypeSet: true,
		Logging: false, LoggingSet: true,
		Retention: 7, RetentionSet: true,
	}); err != nil {
		t.Fatalf("concurrent update: %v", err)
	}

	// A name-only PATCH, carrying a stale view of the other fields, must only
	// change the name.
	if err := sc.UpdateClientKey(ctx, store.UpdateClientKeyInput{
		ID: "ck-1", Name: "renamed", NameSet: true,
	}); err != nil {
		t.Fatalf("metadata-only update: %v", err)
	}

	got := clientEditable(t, sc, "ck-1")
	if got.Name != "renamed" {
		t.Fatalf("name = %q, want renamed", got.Name)
	}
	if got.LoggingEnabled {
		t.Fatal("metadata-only PATCH re-enabled logging set to false concurrently")
	}
	if got.RetentionDays != 7 {
		t.Fatalf("retention = %d, want the concurrently set 7", got.RetentionDays)
	}
}

func seedPatchVirtual(t *testing.T, db *sql.DB, sc *store.Scope, modelID, name string) (providerModelID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO namespaces(account_id,name,kind,entity_id) VALUES(?,'patch-provider','real','patch-provider')`, database.LocalAccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO providers(id,account_id,name,type,base_url,enabled,protocols,created_at,updated_at) VALUES('patch-provider',?,'patch-provider','generic-openai','https://upstream.invalid',1,'["chat"]',?,?)`,
		database.LocalAccountID, database.Now(), database.Now()); err != nil {
		t.Fatal(err)
	}
	providerModelID = "pm-" + modelID
	if _, err := db.Exec(`INSERT INTO provider_models(account_id,id,provider_id,upstream_model_id,display_name,available,origin,first_seen_at,last_seen_at,created_at,updated_at) VALUES(?,'pm-a','patch-provider','model-a','model-a',1,'discovered',?,?,?,?)`,
		database.LocalAccountID, database.Now(), database.Now(), database.Now(), database.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO provider_models(account_id,id,provider_id,upstream_model_id,display_name,available,origin,first_seen_at,last_seen_at,created_at,updated_at) VALUES(?,'pm-b','patch-provider','model-b','model-b',1,'discovered',?,?,?,?)`,
		database.LocalAccountID, database.Now(), database.Now(), database.Now(), database.Now()); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateVirtualModel(ctx, store.CreateVirtualModelInput{
		ID: modelID, NewGroupID: "vg-" + modelID, GroupName: "patch-group", Name: name,
		RoutingMode: "fixed",
		Targets:     []store.VirtualTargetInput{{ProviderModelID: "pm-a", Enabled: true}},
	}); err != nil {
		t.Fatalf("seed virtual model: %v", err)
	}
	return providerModelID
}

// TestUpdateVirtualModelMetadataOnlyPreservesModeAndTargets is the virtual-model
// counterpart: a rename must not restore a stale routing mode or target list.
func TestUpdateVirtualModelMetadataOnlyPreservesModeAndTargets(t *testing.T) {
	sc, db := patchScope(t)
	ctx := context.Background()
	seedPatchVirtual(t, db, sc, "vm-1", "original")

	// A concurrent request switches to ordered fallback with two targets.
	if err := sc.UpdateVirtualModel(ctx, store.UpdateVirtualModelInput{
		ID: "vm-1", ReplaceTargets: true,
		Targets:     []store.VirtualTargetInput{{ProviderModelID: "pm-a", Enabled: true}, {ProviderModelID: "pm-b", Enabled: true}},
		RoutingMode: "ordered_fallback", ModeSet: true,
	}); err != nil {
		t.Fatalf("concurrent mode change: %v", err)
	}

	// A name-only PATCH carrying the stale "fixed" mode must not revert it.
	if err := sc.UpdateVirtualModel(ctx, store.UpdateVirtualModelInput{
		ID: "vm-1", Name: "renamed", NameSet: true,
		RoutingMode: "fixed", ModeSet: false,
	}); err != nil {
		t.Fatalf("metadata-only update: %v", err)
	}

	editable, err := sc.GetVirtualModelEditable(ctx, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if editable.Name != "renamed" {
		t.Fatalf("name = %q, want renamed", editable.Name)
	}
	if editable.RoutingMode != "ordered_fallback" {
		t.Fatalf("routing mode = %q, want ordered_fallback (metadata-only PATCH reverted it)", editable.RoutingMode)
	}
}
