package auth_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// newFastStore creates a SessionStore using the fast test hasher. These tests
// verify session creation/persistence/expiry/CSRF/invalidation semantics, which
// do not require the 64 MiB Argon2id cost; TestSessionStoreProductionHasher in
// session_test.go covers the real KDF path.
func newFastStore(t *testing.T, username, password string, ttl time.Duration) (*auth.SessionStore, *database.DB) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := auth.NewSessionStoreWithHasher(db.SQL, username, password, ttl, fastsecret.Hasher{})
	if err != nil {
		t.Fatal(err)
	}
	return store, db
}

func TestFastSessionCreateGetDelete(t *testing.T) {
	store, _ := newFastStore(t, "admin", "pw", 30*24*time.Hour)
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if session.Token == "" || session.CSRFToken == "" {
		t.Fatal("session missing token or csrf")
	}
	got, ok := store.Get(session.Token)
	if !ok {
		t.Fatal("session not found")
	}
	if got.CSRFToken != session.CSRFToken {
		t.Fatal("csrf mismatch")
	}
	if !store.CheckCSRF(got, session.CSRFToken) {
		t.Fatal("valid csrf rejected")
	}
	if store.CheckCSRF(got, "wrong") {
		t.Fatal("invalid csrf accepted")
	}
	store.Delete(session.Token)
	if _, ok := store.Get(session.Token); ok {
		t.Fatal("deleted session still valid")
	}
}

func TestFastSessionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.NewSessionStoreWithHasher(db.SQL, "admin", "pw", 30*24*time.Hour, fastsecret.Hasher{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := database.Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	store2, err := auth.NewSessionStoreWithHasher(db2.SQL, "admin", "pw", 30*24*time.Hour, fastsecret.Hasher{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := store2.Get(session.Token)
	if !ok {
		t.Fatal("session did not survive restart")
	}
	if got.CSRFToken != session.CSRFToken {
		t.Fatal("csrf did not survive restart")
	}
}

func TestFastSessionSlidingExpiry(t *testing.T) {
	store, db := newFastStore(t, "admin", "pw", time.Hour)
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	selector, _, _ := auth.ParseSessionToken(session.Token)
	half := time.Now().Add(29 * time.Minute)

	if _, err := db.SQL.Exec(`UPDATE admin_sessions SET expires_at=? WHERE id=?`, half.UTC().Format(time.RFC3339Nano), selector); err != nil {
		t.Fatal(err)
	}
	got, ok := store.Get(session.Token)
	if !ok {
		t.Fatal("session not found")
	}
	if !got.ExpiresAt.After(half) {
		t.Fatalf("expiry was not extended: got %v want after %v", got.ExpiresAt, half)
	}
}

func TestFastSessionExpiredRejected(t *testing.T) {
	store, db := newFastStore(t, "admin", "pw", time.Hour)
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	selector, _, _ := auth.ParseSessionToken(session.Token)
	if _, err := db.SQL.Exec(`UPDATE admin_sessions SET expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), selector); err != nil {

		t.Fatal(err)
	}
	if _, ok := store.Get(session.Token); ok {
		t.Fatal("expired session accepted")
	}
}

func TestFastCredentialChangeInvalidatesSessions(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := auth.NewSessionStoreWithHasher(db.SQL, "admin", "oldpw", 30*24*time.Hour, fastsecret.Hasher{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	store2, err := auth.NewSessionStoreWithHasher(db.SQL, "admin", "newpw", 30*24*time.Hour, fastsecret.Hasher{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store2.Get(session.Token); ok {
		t.Fatal("session survived credential change")
	}
}

func TestFastMultipleSessionsCoexist(t *testing.T) {
	store, _ := newFastStore(t, "admin", "pw", 30*24*time.Hour)
	s1, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get(s1.Token); !ok {
		t.Fatal("session 1 not valid")
	}
	if _, ok := store.Get(s2.Token); !ok {
		t.Fatal("session 2 not valid")
	}
	store.Delete(s1.Token)
	if _, ok := store.Get(s1.Token); ok {
		t.Fatal("session 1 still valid after delete")
	}
	if _, ok := store.Get(s2.Token); !ok {
		t.Fatal("session 2 invalidated by deleting session 1")
	}
}

func TestFastSessionSecretNotStoredInPlaintext(t *testing.T) {
	store, db := newFastStore(t, "admin", "pw", 30*24*time.Hour)
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_, secret, _ := auth.ParseSessionToken(session.Token)
	var tokenHash string
	if err := db.SQL.QueryRow(`SELECT token_hash FROM admin_sessions`).Scan(&tokenHash); err != nil {
		t.Fatal(err)
	}
	if tokenHash == secret {
		t.Fatal("raw session secret stored in database")
	}
	// The stored hash must verify against the secret via the store's Get,
	// which exercises the fast hasher's Verify path.
	if _, ok := store.Get(session.Token); !ok {
		t.Fatal("stored hash does not verify against the session secret")
	}
}

func TestFastInvalidTokenRejected(t *testing.T) {
	store, _ := newFastStore(t, "admin", "pw", 30*24*time.Hour)
	if _, ok := store.Get(""); ok {
		t.Fatal("empty token accepted")
	}
	if _, ok := store.Get("no-dot-token"); ok {
		t.Fatal("malformed token accepted")
	}
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	store.Delete(session.Token)
	if _, ok := store.Get(session.Token); ok {
		t.Fatal("deleted session accepted")
	}
}
