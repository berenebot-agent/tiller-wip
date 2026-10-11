package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
)

func newAPITokenStore(t *testing.T) (*Store, *database.DB) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, id := range []string{"account-a", "account-b"} {
		if _, err := db.SQL.Exec(`INSERT INTO accounts(id,plan,status,created_at,updated_at) VALUES(?,'free','active',?,?)`, id, database.Now(), database.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return New(db.SQL), db
}

func insertStoreManagementToken(t *testing.T, scope *Scope, id, name, selector, scopes string, expires sql.NullString) {
	t.Helper()
	err := scope.CreateManagementToken(context.Background(), CreateManagementTokenInput{
		ID:          id,
		Name:        name,
		Description: "test token",
		Selector:    selector,
		Hash:        "hash-" + selector,
		Fingerprint: "fp-" + selector,
		Scopes:      scopes,
		ExpiresAt:   expires,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestManagementTokensAreAccountScoped(t *testing.T) {
	store, _ := newAPITokenStore(t)
	ctx := context.Background()
	scopeA := store.For("account-a")
	scopeB := store.For("account-b")

	insertStoreManagementToken(t, scopeA, "tok-a", "alpha", "selector-a", `{"providers":2}`, sql.NullString{})

	rowsA, err := scopeA.ListManagementTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsA) != 1 || rowsA[0].Name != "alpha" || rowsA[0].Scopes != `{"providers":2}` {
		t.Fatalf("account A rows = %+v", rowsA)
	}
	rowsB, err := scopeB.ListManagementTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsB) != 0 {
		t.Fatalf("account B saw %d rows", len(rowsB))
	}

	// Cross-account reads and mutations are invisible (not-found).
	if _, err := scopeB.GetManagementToken(ctx, "tok-a"); !errors.Is(err, ErrManagementTokenNotFound) {
		t.Fatalf("cross-account get = %v", err)
	}
	if err := scopeB.UpdateManagementToken(ctx, UpdateManagementTokenInput{ID: "tok-a", Name: "hijack", Scopes: "{}"}); !errors.Is(err, ErrManagementTokenNotFound) {
		t.Fatalf("cross-account update = %v", err)
	}
	if found, err := scopeB.RotateManagementToken(ctx, "tok-a", "s", "h", "f"); err != nil || found {
		t.Fatalf("cross-account rotate = %v, %v", found, err)
	}
	if found, err := scopeB.DeleteManagementToken(ctx, "tok-a"); err != nil || found {
		t.Fatalf("cross-account delete = %v, %v", found, err)
	}
	if count, err := scopeB.RevokeAllManagementTokens(ctx); err != nil || count != 0 {
		t.Fatalf("cross-account revoke-all = %d, %v", count, err)
	}

	// The platform-level selector lookup is how authentication resolves the
	// account; it must find the row regardless of scope.
	row, err := store.ManagementTokenBySelector(ctx, "selector-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != "tok-a" || row.AccountID != "account-a" || !row.Enabled || row.AccountStatus != "active" {
		t.Fatalf("selector row = %+v", row)
	}

	// Account A can update, rotate, and delete its own token.
	if err := scopeA.UpdateManagementToken(ctx, UpdateManagementTokenInput{ID: "tok-a", Name: "renamed", Description: "d", Scopes: `{"virtual":1}`, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if found, err := scopeA.RotateManagementToken(ctx, "tok-a", "selector-b", "hash-b", "fp-b"); err != nil || !found {
		t.Fatalf("rotate = %v, %v", found, err)
	}
	rotated, err := scopeA.GetManagementToken(ctx, "tok-a")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Name != "renamed" || rotated.Fingerprint != "fp-b" || !rotated.RotatedAt.Valid {
		t.Fatalf("rotated row = %+v", rotated)
	}
	if found, err := scopeA.DeleteManagementToken(ctx, "tok-a"); err != nil || !found {
		t.Fatalf("delete = %v, %v", found, err)
	}
}

func TestRevokeAllManagementTokensCounts(t *testing.T) {
	store, _ := newAPITokenStore(t)
	ctx := context.Background()
	scope := store.For("account-a")
	insertStoreManagementToken(t, scope, "tok-1", "one", "sel-1", `{}`, sql.NullString{})
	insertStoreManagementToken(t, scope, "tok-2", "two", "sel-2", `{}`, sql.NullString{})
	count, err := scope.RevokeAllManagementTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("revoked = %d", count)
	}
	rows, err := scope.ListManagementTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows after revoke-all = %d", len(rows))
	}
}
