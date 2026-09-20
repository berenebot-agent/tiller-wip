package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

func openLegalStore(t *testing.T) (*database.DB, *store.Store) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, store.New(db.SQL)
}

// TestSeedLegalDocRefreshesPlaceholderButNotOperatorEdit proves the deploy-time
// seed rule: a row still carrying the migration placeholder (updated_by NULL)
// is refreshed, while a row an operator has edited is preserved.
func TestSeedLegalDocRefreshesPlaceholderButNotOperatorEdit(t *testing.T) {
	_, st := openLegalStore(t)
	ctx := context.Background()

	// The migration seeds terms/privacy with updated_by NULL. A seed must
	// replace that placeholder.
	if err := st.SeedLegalDoc(ctx, store.LegalDoc{Slug: "terms", Title: "Terms of Service", Body: "seed draft"}); err != nil {
		t.Fatal(err)
	}
	seeded, err := st.GetLegalDoc(ctx, "terms")
	if err != nil {
		t.Fatal(err)
	}
	if seeded.Body != "seed draft" || seeded.UpdatedBy != "" {
		t.Fatalf("placeholder not refreshed by seed: %+v", seeded)
	}

	// An operator edit sets updated_by, so a later deploy seed must not clobber
	// it.
	if err := st.UpsertLegalDoc(ctx, store.LegalDoc{Slug: "terms", Title: "Terms of Service", Body: "operator edit"}, "platform"); err != nil {
		t.Fatal(err)
	}
	if err := st.SeedLegalDoc(ctx, store.LegalDoc{Slug: "terms", Title: "Terms of Service", Body: "newer deploy draft"}); err != nil {
		t.Fatal(err)
	}
	after, err := st.GetLegalDoc(ctx, "terms")
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != "operator edit" || after.UpdatedBy != "platform" {
		t.Fatalf("seed overwrote an operator edit: %+v", after)
	}

	// A slug with no row at all is inserted by SeedLegalDoc.
	if err := st.SeedLegalDoc(ctx, store.LegalDoc{Slug: "aup", Title: "Acceptable Use Policy", Body: "aup draft"}); err != nil {
		t.Fatal(err)
	}
	aup, err := st.GetLegalDoc(ctx, "aup")
	if err != nil || aup.Body != "aup draft" || aup.UpdatedBy != "" {
		t.Fatalf("seed did not insert new slug: %+v err=%v", aup, err)
	}
}

func TestGetLegalDocUnknownSlug(t *testing.T) {
	_, st := openLegalStore(t)
	_, err := st.GetLegalDoc(context.Background(), "does-not-exist")
	if !errors.Is(err, store.ErrLegalDocNotFound) {
		t.Fatalf("unknown slug error = %v, want ErrLegalDocNotFound", err)
	}
}

func TestListLegalDocsOrdersBySlug(t *testing.T) {
	db, st := openLegalStore(t)
	ctx := context.Background()
	if _, err := db.SQL.Exec(`INSERT INTO legal_documents(slug,title,body,updated_at,updated_by) VALUES('security','Security','s','2026-09-21T00:00:00Z','platform')`); err != nil {
		t.Fatal(err)
	}
	docs, err := st.ListLegalDocs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) < 3 {
		t.Fatalf("expected at least 3 docs, got %d", len(docs))
	}
	for i := 1; i < len(docs); i++ {
		if docs[i-1].Slug > docs[i].Slug {
			t.Fatalf("docs not slug-ordered: %q before %q", docs[i-1].Slug, docs[i].Slug)
		}
	}
}

// TestLegalAcceptanceRoundTrips proves an acceptance record written through the
// account-scoped handle is readable back with its timestamps intact.
func TestLegalAcceptanceRoundTrips(t *testing.T) {
	_, st := openLegalStore(t)
	ctx := context.Background()
	const userID = "user-legal-test"
	acceptance := store.LegalAcceptance{
		ID:               "acc-1",
		UserID:           userID,
		TermsUpdatedAt:   "2026-09-21T00:00:00Z",
		PrivacyUpdatedAt: "2026-09-21T00:00:00Z",
		AcceptedAt:       "2026-09-21T00:00:00Z",
		IP:               "203.0.113.5",
		UserAgent:        "test-agent",
	}
	if err := st.For(database.LocalAccountID).RecordLegalAcceptance(ctx, acceptance.ID, acceptance); err != nil {
		t.Fatal(err)
	}
	got, err := st.LatestLegalAcceptance(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != acceptance.ID || got.UserID != userID || got.TermsUpdatedAt != acceptance.TermsUpdatedAt || got.PrivacyUpdatedAt != acceptance.PrivacyUpdatedAt || got.AcceptedAt != acceptance.AcceptedAt {
		t.Fatalf("acceptance mismatch: %+v", got)
	}
}
