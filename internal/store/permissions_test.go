package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

// TestListPermissionsGroupsModelsPerProvider guards the batched permission
// read: one query returns every group's models, and each row must land in its
// own group with the right feeder default and canonical name.
func TestListPermissionsGroupsModelsPerProvider(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	sc := store.New(db.SQL).For(database.LocalAccountID)

	if err := sc.CreateClientKey(ctx, store.CreateClientKeyInput{ID: "ck-1", Name: "Client one", Selector: "sk-tr-aaaa", Hash: "hash", Fingerprint: "fp", Type: "catalogue"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ id, name string }{{"prov-b", "provider-b"}, {"prov-a", "provider-a"}} {
		if err := sc.CreateProvider(ctx, store.CreateProviderInput{ID: p.id, Name: p.name, Type: "openai", BaseURL: "https://" + p.name + ".test/v1", Enabled: true, Protocols: `["chat"]`}); err != nil {
			t.Fatal(err)
		}
	}
	// provider-a's feeder default is on, so its models must come back enabled.
	if err := sc.UpdatePermissions(ctx, "ck-1", []store.PermissionDefaultUpdate{{Kind: "real", GroupID: "prov-a", Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := sc.ApplyCatalogue(ctx, "prov-a", []store.CatalogueModel{{ID: "model-a2"}, {ID: "model-a1"}}); err != nil {
		t.Fatal(err)
	}
	if err := sc.ApplyCatalogue(ctx, "prov-b", []store.CatalogueModel{{ID: "model-b1"}}); err != nil {
		t.Fatal(err)
	}
	var a1ID string
	if err := db.SQL.QueryRowContext(ctx, `SELECT id FROM provider_models WHERE account_id=? AND provider_id='prov-a' AND upstream_model_id='model-a1'`, database.LocalAccountID).Scan(&a1ID); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateVirtualModel(ctx, store.CreateVirtualModelInput{ID: "vm-1", NewGroupID: "vg-1", GroupName: "vgroup", Name: "coding", RoutingMode: "ordered_fallback", Targets: []store.VirtualTargetInput{{ProviderModelID: a1ID, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}

	groups, err := sc.ListPermissions(ctx, "ck-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3 (two real providers then the virtual group): %+v", len(groups), groups)
	}
	want := []struct {
		kind, id, name string
		feeder         bool
		models         []struct {
			name    string
			enabled bool
		}
	}{
		{"real", "prov-a", "provider-a", true, []struct {
			name    string
			enabled bool
		}{{"provider-a/model-a1", true}, {"provider-a/model-a2", true}}},
		{"real", "prov-b", "provider-b", false, []struct {
			name    string
			enabled bool
		}{{"provider-b/model-b1", false}}},
		{"virtual", "vg-1", "vgroup", false, []struct {
			name    string
			enabled bool
		}{{"vgroup/coding", false}}},
	}
	for i, w := range want {
		g := groups[i]
		if g.Kind != w.kind || g.ID != w.id || g.Name != w.name || g.NewModelsEnabled != w.feeder {
			t.Fatalf("group %d = %+v, want kind=%s id=%s name=%s feeder=%v", i, g, w.kind, w.id, w.name, w.feeder)
		}
		if len(g.Models) != len(w.models) {
			t.Fatalf("group %s models = %+v, want %d", g.ID, g.Models, len(w.models))
		}
		for j, wm := range w.models {
			if g.Models[j].CanonicalModelID != wm.name || g.Models[j].Enabled != wm.enabled {
				t.Fatalf("group %s model %d = %+v, want name=%s enabled=%v", g.ID, j, g.Models[j], wm.name, wm.enabled)
			}
		}
	}
}
