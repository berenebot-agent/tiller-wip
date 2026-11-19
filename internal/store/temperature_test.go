package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/database/dbtest"
	"github.com/tiller-router/tiller-router/internal/store"
)

func TestTemperatureCatalogueRefresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := dbtest.Open(ctx, filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sc := store.New(db.SQL).For(database.LocalAccountID)
	if err := sc.CreateProvider(ctx, store.CreateProviderInput{ID: "p", Name: "provider", Type: "anthropic", BaseURL: "https://example.test/v1", Enabled: true, Protocols: `["messages"]`}); err != nil {
		t.Fatal(err)
	}
	var modelID string
	yes, no := true, false
	for _, support := range []*bool{nil, &no, &yes, nil} {
		if err := sc.ApplyCatalogue(ctx, "p", []store.CatalogueModel{{ID: "m", SupportsTemperature: support}}); err != nil {
			t.Fatal(err)
		}
		models, err := sc.ListModels(ctx, store.ModelFilter{ProviderID: "p", Limit: 10})
		if err != nil || len(models) != 1 {
			t.Fatalf("models: count=%d err=%v", len(models), err)
		}
		if modelID != "" && models[0].ID != modelID {
			t.Fatal("refresh replaced model identity")
		}
		modelID = models[0].ID
		target, err := sc.RealRouteTarget(ctx, modelID)
		if err != nil {
			t.Fatal(err)
		}
		got := target.SupportsTemperature
		if got.Valid != (support != nil) || support != nil && (got.Int64 != 0) != *support {
			t.Fatalf("stored support = %+v, want %v", got, support)
		}
	}
}
