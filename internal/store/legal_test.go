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

// TestSeedLegalDocRefreshesPlaceholderButNotOperatorEdit proves the re-seed rule
// the router relies on at every boot: a row still carrying the migration
// placeholder (updated_by NULL) is refreshed with the generated placeholder,
// while a row an operator has published is preserved.
func TestSeedLegalDocRefreshesPlaceholderButNotOperatorEdit(t *testing.T) {
	_, st := openLegalStore(t)
	ctx := context.Background()

	// The migration seeds terms/privacy with updated_by NULL. A seed must
	// replace that placeholder.
	if err := st.SeedLegalDoc(ctx, store.LegalDoc{Slug: "terms", Title: "Terms of Service", Body: "generated placeholder"}); err != nil {
		t.Fatal(err)
	}
	seeded, err := st.GetLegalDoc(ctx, "terms")
	if err != nil {
		t.Fatal(err)
	}
	if seeded.Body != "generated placeholder" || seeded.UpdatedBy != "" {
		t.Fatalf("placeholder not refreshed by seed: %+v", seeded)
	}

	// An operator edit sets updated_by, so a later deploy seed must not clobber
	// it.
	if err := st.UpsertLegalDoc(ctx, store.LegalDoc{Slug: "terms", Title: "Terms of Service", Body: "operator publish"}, "platform"); err != nil {
		t.Fatal(err)
	}
	if err := st.SeedLegalDoc(ctx, store.LegalDoc{Slug: "terms", Title: "Terms of Service", Body: "newer generated placeholder"}); err != nil {
		t.Fatal(err)
	}
	after, err := st.GetLegalDoc(ctx, "terms")
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != "operator publish" || after.UpdatedBy != "platform" {
		t.Fatalf("seed overwrote an operator publish: %+v", after)
	}

	// A slug with no row at all is inserted by the seed.
	if err := st.SeedLegalDoc(ctx, store.LegalDoc{Slug: "extra-doc", Title: "Extra", Body: "extra placeholder"}); err != nil {
		t.Fatal(err)
	}
	extra, err := st.GetLegalDoc(ctx, "extra-doc")
	if err != nil || extra.Body != "extra placeholder" || extra.UpdatedBy != "" {
		t.Fatalf("seed did not insert new slug: %+v err=%v", extra, err)
	}
}

// TestUpsertLegalDocAlwaysReplaces proves a publish is unconditional: it
// replaces whatever was stored and stamps updated_by, with no conditional on the
// previous row.
func TestUpsertLegalDocAlwaysReplaces(t *testing.T) {
	_, st := openLegalStore(t)
	ctx := context.Background()

	if err := st.UpsertLegalDoc(ctx, store.LegalDoc{Slug: "terms", Title: "Terms of Service", Body: "first publish"}, "platform"); err != nil {
		t.Fatal(err)
	}
	published, err := st.GetLegalDoc(ctx, "terms")
	if err != nil {
		t.Fatal(err)
	}
	if published.Body != "first publish" || published.UpdatedBy != "platform" {
		t.Fatalf("publish did not replace the placeholder: %+v", published)
	}

	if err := st.UpsertLegalDoc(ctx, store.LegalDoc{Slug: "terms", Title: "Terms of Service", Body: "second publish"}, "platform"); err != nil {
		t.Fatal(err)
	}
	after, err := st.GetLegalDoc(ctx, "terms")
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != "second publish" || after.UpdatedBy != "platform" {
		t.Fatalf("later publish did not replace the earlier one: %+v", after)
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
	if _, err := db.SQL.Exec(`INSERT INTO legal_documents(slug,title,body,updated_at,updated_by) VALUES('extra-doc','Extra','s','2026-09-21T00:00:00Z','platform')`); err != nil {
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
