package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

func openTemplateStore(t *testing.T) (*database.DB, *store.Store) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, store.New(db.SQL)
}

// matrixOf flattens a permission matrix into "default:kind:group" and
// "model:kind:id" keys so two matrices can be compared exactly.
func matrixOf(groups []store.PermissionGroupRow) map[string]bool {
	out := map[string]bool{}
	for _, g := range groups {
		out["default:"+g.Kind+":"+g.ID] = g.NewModelsEnabled
		for _, m := range g.Models {
			out["model:"+m.Kind+":"+m.ID] = m.Enabled
		}
	}
	return out
}

func clientMatrix(t *testing.T, sc *store.Scope, id string) map[string]bool {
	t.Helper()
	groups, err := sc.ListPermissions(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return matrixOf(groups)
}

func templateMatrix(t *testing.T, sc *store.Scope, id string) map[string]bool {
	t.Helper()
	groups, err := sc.ListTemplatePermissions(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return matrixOf(groups)
}

// assertMirrors fails unless the client's matrix equals the template's.
func assertMirrors(t *testing.T, sc *store.Scope, clientID, templateID, step string) {
	t.Helper()
	got, want := clientMatrix(t, sc, clientID), templateMatrix(t, sc, templateID)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: client %s matrix %v != template %s matrix %v", step, clientID, got, templateID, want)
	}
}

func modelID(t *testing.T, db *database.DB, providerID, upstream string) string {
	t.Helper()
	var id string
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE provider_id=? AND upstream_model_id=?`, providerID, upstream).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func catalogueNames(t *testing.T, sc *store.Scope, clientID string) []string {
	t.Helper()
	entries, err := sc.ListCatalogueEntries(context.Background(), clientID)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range entries {
		out = append(out, e.Canonical)
	}
	sort.Strings(out)
	return out
}

// TestTemplateLinkedClientsMirrorTemplate walks every path that writes a
// permission matrix and checks a linked client always equals its template,
// while a Custom client is never touched by template changes.
func TestTemplateLinkedClientsMirrorTemplate(t *testing.T) {
	t.Parallel()
	db, st := openTemplateStore(t)
	ctx := context.Background()
	sc := st.For(database.LocalAccountID)

	if err := sc.CreateProvider(ctx, store.CreateProviderInput{ID: "prov-a", Name: "provider-a", Type: "openai", BaseURL: "https://a.test/v1", Enabled: true, Protocols: `["chat"]`}); err != nil {
		t.Fatal(err)
	}
	if err := sc.ApplyCatalogue(ctx, "prov-a", []store.CatalogueModel{{ID: "m1"}, {ID: "m2"}}); err != nil {
		t.Fatal(err)
	}
	m1, m2 := modelID(t, db, "prov-a", "m1"), modelID(t, db, "prov-a", "m2")

	if err := sc.CreateTemplate(ctx, store.CreateTemplateInput{ID: "tpl-1", Name: "Coding agents", StartFrom: store.TemplateStartEmpty}); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateClientKey(ctx, store.CreateClientKeyInput{ID: "ck-linked", Name: "linked", Selector: "sk-tr-l", Hash: "h", Fingerprint: "f", Type: "catalogue", TemplateID: "tpl-1"}); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateClientKey(ctx, store.CreateClientKeyInput{ID: "ck-custom", Name: "custom", Selector: "sk-tr-c", Hash: "h", Fingerprint: "f", Type: "catalogue"}); err != nil {
		t.Fatal(err)
	}
	assertMirrors(t, sc, "ck-linked", "tpl-1", "create")
	customBefore := clientMatrix(t, sc, "ck-custom")

	// Template save fans out to the linked client only.
	linked, err := sc.UpdateTemplatePermissions(ctx, "tpl-1", []store.PermissionDefaultUpdate{{Kind: "real", GroupID: "prov-a", Enabled: true}}, []store.PermissionModelUpdate{{Kind: "real", ModelID: m1, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(linked, []string{"ck-linked"}) {
		t.Fatalf("linked = %v", linked)
	}
	assertMirrors(t, sc, "ck-linked", "tpl-1", "template save")
	if got := catalogueNames(t, sc, "ck-linked"); !reflect.DeepEqual(got, []string{"provider-a/m1"}) {
		t.Fatalf("linked catalogue = %v", got)
	}
	if got := clientMatrix(t, sc, "ck-custom"); !reflect.DeepEqual(got, customBefore) {
		t.Fatalf("custom client changed by template save: %v != %v", got, customBefore)
	}

	// Seeding: a discovered model, a manual model, a new provider, and a
	// virtual model in a new group all land identically.
	if err := sc.ApplyCatalogue(ctx, "prov-a", []store.CatalogueModel{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}); err != nil {
		t.Fatal(err)
	}
	assertMirrors(t, sc, "ck-linked", "tpl-1", "discovered model")
	if !templateMatrix(t, sc, "tpl-1")["model:real:"+modelID(t, db, "prov-a", "m3")] {
		t.Fatal("template feeder default did not enable the newly discovered model")
	}
	if err := sc.InsertManualModel(ctx, "prov-a", "manual-1", store.CatalogueModel{ID: "manual"}); err != nil {
		t.Fatal(err)
	}
	assertMirrors(t, sc, "ck-linked", "tpl-1", "manual model")
	if err := sc.CreateProvider(ctx, store.CreateProviderInput{ID: "prov-b", Name: "provider-b", Type: "openai", BaseURL: "https://b.test/v1", Enabled: true, Protocols: `["chat"]`}); err != nil {
		t.Fatal(err)
	}
	if err := sc.ApplyCatalogue(ctx, "prov-b", []store.CatalogueModel{{ID: "b1"}}); err != nil {
		t.Fatal(err)
	}
	assertMirrors(t, sc, "ck-linked", "tpl-1", "new provider")
	if err := sc.CreateVirtualModel(ctx, store.CreateVirtualModelInput{ID: "vm-1", NewGroupID: "vg-1", GroupName: "vgroup", Name: "coding", RoutingMode: "ordered_fallback", Targets: []store.VirtualTargetInput{{ProviderModelID: m2, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	assertMirrors(t, sc, "ck-linked", "tpl-1", "virtual model")

	// Deletes remove template rows too.
	if err := sc.DeleteManualModel(ctx, "manual-1"); err != nil {
		t.Fatal(err)
	}
	if err := sc.DeleteVirtualModel(ctx, "vm-1"); err != nil {
		t.Fatal(err)
	}
	if err := sc.DeleteVirtualGroup(ctx, "vg-1"); err != nil {
		t.Fatal(err)
	}
	if err := sc.DeleteProvider(ctx, "prov-b"); err != nil {
		t.Fatal(err)
	}
	assertMirrors(t, sc, "ck-linked", "tpl-1", "deletes")
	for _, q := range []string{
		`SELECT count(*) FROM catalogue_template_model_permissions WHERE model_id IN ('manual-1','vm-1')`,
		`SELECT count(*) FROM catalogue_template_group_defaults WHERE group_id IN ('vg-1','prov-b')`,
		`SELECT count(*) FROM catalogue_template_model_permissions x WHERE NOT EXISTS (SELECT 1 FROM provider_models m WHERE m.id=x.model_id) AND x.model_kind='real'`,
	} {
		var n int
		if err := db.SQL.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s = %d orphaned template rows", q, n)
		}
	}

	// A linked client's matrix cannot be edited directly.
	if err := sc.UpdatePermissions(ctx, "ck-linked", nil, []store.PermissionModelUpdate{{Kind: "real", ModelID: m2, Enabled: true}}); !errors.Is(err, store.ErrClientLinkedToTemplate) {
		t.Fatalf("direct update on linked client = %v, want ErrClientLinkedToTemplate", err)
	}

	// Detach keeps the current access, then the client is editable.
	before := clientMatrix(t, sc, "ck-linked")
	if err := sc.UpdateClientKey(ctx, store.UpdateClientKeyInput{ID: "ck-linked", TemplateSet: true}); err != nil {
		t.Fatal(err)
	}
	if got := clientMatrix(t, sc, "ck-linked"); !reflect.DeepEqual(got, before) {
		t.Fatalf("detach changed access: %v != %v", got, before)
	}
	if err := sc.UpdatePermissions(ctx, "ck-linked", nil, []store.PermissionModelUpdate{{Kind: "real", ModelID: m2, Enabled: true}}); err != nil {
		t.Fatalf("update after detach = %v", err)
	}
	if _, err := sc.UpdateTemplatePermissions(ctx, "tpl-1", nil, []store.PermissionModelUpdate{{Kind: "real", ModelID: m1, Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	if !clientMatrix(t, sc, "ck-linked")["model:real:"+m1] {
		t.Fatal("detached client followed a later template change")
	}

	// Linking a Custom client overwrites its matrix with the template's.
	if err := sc.UpdateClientKey(ctx, store.UpdateClientKeyInput{ID: "ck-custom", TemplateSet: true, TemplateID: "tpl-1"}); err != nil {
		t.Fatal(err)
	}
	assertMirrors(t, sc, "ck-custom", "tpl-1", "link")

	// Deleting the template detaches its clients without changing access.
	before = clientMatrix(t, sc, "ck-custom")
	detached, err := sc.DeleteTemplate(ctx, "tpl-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(detached, []string{"ck-custom"}) {
		t.Fatalf("detached = %v", detached)
	}
	if got := clientMatrix(t, sc, "ck-custom"); !reflect.DeepEqual(got, before) {
		t.Fatalf("template delete changed access: %v != %v", got, before)
	}
	editable, err := sc.GetClientKeyEditable(ctx, "ck-custom")
	if err != nil {
		t.Fatal(err)
	}
	if editable.TemplateID != "" {
		t.Fatalf("template_id after delete = %q", editable.TemplateID)
	}
	var rows int
	if err := db.SQL.QueryRow(`SELECT (SELECT count(*) FROM catalogue_template_model_permissions)+(SELECT count(*) FROM catalogue_template_group_defaults)`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("template child rows after delete = %d", rows)
	}
}

// TestCreateTemplateStartFrom checks the All-models and Copy-of-client
// starting sets.
func TestCreateTemplateStartFrom(t *testing.T) {
	t.Parallel()
	db, st := openTemplateStore(t)
	ctx := context.Background()
	sc := st.For(database.LocalAccountID)
	if err := sc.CreateProvider(ctx, store.CreateProviderInput{ID: "prov-a", Name: "provider-a", Type: "openai", BaseURL: "https://a.test/v1", Enabled: true, Protocols: `["chat"]`}); err != nil {
		t.Fatal(err)
	}
	if err := sc.ApplyCatalogue(ctx, "prov-a", []store.CatalogueModel{{ID: "m1"}, {ID: "m2"}}); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateClientKey(ctx, store.CreateClientKeyInput{ID: "ck-1", Name: "one", Selector: "sk-tr-1", Hash: "h", Fingerprint: "f", Type: "catalogue"}); err != nil {
		t.Fatal(err)
	}
	if err := sc.UpdatePermissions(ctx, "ck-1", nil, []store.PermissionModelUpdate{{Kind: "real", ModelID: modelID(t, db, "prov-a", "m2"), Enabled: true}}); err != nil {
		t.Fatal(err)
	}

	if err := sc.CreateTemplate(ctx, store.CreateTemplateInput{ID: "tpl-all", Name: "All", StartFrom: store.TemplateStartAll}); err != nil {
		t.Fatal(err)
	}
	for key, enabled := range templateMatrix(t, sc, "tpl-all") {
		if !enabled {
			t.Fatalf("all-models template has %s disabled", key)
		}
	}
	if err := sc.CreateTemplate(ctx, store.CreateTemplateInput{ID: "tpl-copy", Name: "Copy", StartFrom: store.TemplateStartClient, SourceClientID: "ck-1"}); err != nil {
		t.Fatal(err)
	}
	if got, want := templateMatrix(t, sc, "tpl-copy"), clientMatrix(t, sc, "ck-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("copied template %v != client %v", got, want)
	}
	if err := sc.CreateTemplate(ctx, store.CreateTemplateInput{ID: "tpl-dup", Name: "Copy", StartFrom: store.TemplateStartEmpty}); !database.IsConstraint(err) {
		t.Fatalf("duplicate name = %v, want constraint error", err)
	}
	rows, err := sc.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "All" || rows[1].Name != "Copy" {
		t.Fatalf("templates = %+v", rows)
	}
}

// TestTemplatesAreAccountScoped checks one account can never see, edit,
// delete or link another account's template, nor copy another account's
// client into a template.
func TestTemplatesAreAccountScoped(t *testing.T) {
	t.Parallel()
	db, st := openTemplateStore(t)
	ctx := context.Background()
	for _, id := range []string{"account-a", "account-b"} {
		if _, err := db.SQL.Exec(`INSERT INTO accounts(id,plan,status,created_at,updated_at) VALUES(?,'free','active',?,?)`, id, database.Now(), database.Now()); err != nil {
			t.Fatal(err)
		}
	}
	a, b := st.For("account-a"), st.For("account-b")
	if err := a.CreateTemplate(ctx, store.CreateTemplateInput{ID: "tpl-a", Name: "Shared name", StartFrom: store.TemplateStartEmpty}); err != nil {
		t.Fatal(err)
	}
	if err := a.CreateClientKey(ctx, store.CreateClientKeyInput{ID: "ck-a", Name: "a", Selector: "sk-tr-a", Hash: "h", Fingerprint: "f", Type: "catalogue"}); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateClientKey(ctx, store.CreateClientKeyInput{ID: "ck-b", Name: "b", Selector: "sk-tr-b", Hash: "h", Fingerprint: "f", Type: "catalogue"}); err != nil {
		t.Fatal(err)
	}

	if rows, err := b.ListTemplates(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("account B templates = %+v, %v", rows, err)
	}
	if _, err := b.GetTemplate(ctx, "tpl-a"); !errors.Is(err, store.ErrTemplateNotFound) {
		t.Fatalf("cross-account get = %v", err)
	}
	if _, err := b.ListTemplatePermissions(ctx, "tpl-a"); !errors.Is(err, store.ErrTemplateNotFound) {
		t.Fatalf("cross-account permissions read = %v", err)
	}
	if _, err := b.UpdateTemplatePermissions(ctx, "tpl-a", nil, nil); !errors.Is(err, store.ErrTemplateNotFound) {
		t.Fatalf("cross-account permissions update = %v", err)
	}
	name := "hijack"
	if err := b.UpdateTemplate(ctx, "tpl-a", &name, nil); !errors.Is(err, store.ErrTemplateNotFound) {
		t.Fatalf("cross-account rename = %v", err)
	}
	if _, err := b.DeleteTemplate(ctx, "tpl-a"); !errors.Is(err, store.ErrTemplateNotFound) {
		t.Fatalf("cross-account delete = %v", err)
	}
	if err := b.UpdateClientKey(ctx, store.UpdateClientKeyInput{ID: "ck-b", TemplateSet: true, TemplateID: "tpl-a"}); !errors.Is(err, store.ErrTemplateNotFound) {
		t.Fatalf("cross-account link = %v", err)
	}
	if err := b.CreateClientKey(ctx, store.CreateClientKeyInput{ID: "ck-b2", Name: "b2", Selector: "sk-tr-b2", Hash: "h", Fingerprint: "f", Type: "catalogue", TemplateID: "tpl-a"}); !errors.Is(err, store.ErrTemplateNotFound) {
		t.Fatalf("cross-account create link = %v", err)
	}
	if err := b.CreateTemplate(ctx, store.CreateTemplateInput{ID: "tpl-b", Name: "Shared name", StartFrom: store.TemplateStartClient, SourceClientID: "ck-a"}); !errors.Is(err, store.ErrClientKeyNotFound) {
		t.Fatalf("cross-account copy source = %v", err)
	}
	// Names are unique per account, not globally.
	if err := b.CreateTemplate(ctx, store.CreateTemplateInput{ID: "tpl-b", Name: "Shared name", StartFrom: store.TemplateStartEmpty}); err != nil {
		t.Fatalf("same name in another account = %v", err)
	}
	if got, err := a.GetTemplate(ctx, "tpl-a"); err != nil || got.Name != "Shared name" {
		t.Fatalf("account A template after B's attempts = %+v, %v", got, err)
	}
}
