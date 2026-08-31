package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

func newTestStore(t *testing.T, username, password string, ttl time.Duration) (*SessionStore, *database.DB) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewSessionStore(db.SQL, username, password, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return store, db
}

func TestSessionCreateGetDelete(t *testing.T) {
	store, _ := newTestStore(t, "admin", "pw", 30*24*time.Hour)
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

func TestSessionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSessionStore(db.SQL, "admin", "pw", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Reopen the same DB file and build a fresh store, simulating a restart.
	db2, err := database.Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	store2, err := NewSessionStore(db2.SQL, "admin", "pw", 30*24*time.Hour)
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

func TestSessionSlidingExpiry(t *testing.T) {
	store, db := newTestStore(t, "admin", "pw", time.Hour)
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	selector, _, _ := parseSessionToken(session.Token)
	// Force the expiry to just past the half-lifetime threshold (ttl/2 = 30m),
	// so the next Get extends it.
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

func TestSessionExpiredRejected(t *testing.T) {
	store, db := newTestStore(t, "admin", "pw", time.Hour)
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	selector, _, _ := parseSessionToken(session.Token)
	if _, err := db.SQL.Exec(`UPDATE admin_sessions SET expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), selector); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get(session.Token); ok {
		t.Fatal("expired session accepted")
	}
}

func TestCredentialChangeInvalidatesSessions(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewSessionStore(db.SQL, "admin", "oldpw", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a restart with changed credentials.
	store2, err := NewSessionStore(db.SQL, "admin", "newpw", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store2.Get(session.Token); ok {
		t.Fatal("session survived credential change")
	}
}

func TestMultipleSessionsCoexist(t *testing.T) {
	store, _ := newTestStore(t, "admin", "pw", 30*24*time.Hour)
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

func TestSessionSecretNotStoredInPlaintext(t *testing.T) {
	store, db := newTestStore(t, "admin", "pw", 30*24*time.Hour)
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_, secret, _ := parseSessionToken(session.Token)
	var tokenHash string
	if err := db.SQL.QueryRow(`SELECT token_hash FROM admin_sessions`).Scan(&tokenHash); err != nil {
		t.Fatal(err)
	}
	if tokenHash == secret {
		t.Fatal("raw session secret stored in database")
	}
	if !VerifySecret(secret, tokenHash) {
		t.Fatal("stored hash does not verify against the session secret")
	}
}

func TestInvalidTokenRejected(t *testing.T) {
	store, _ := newTestStore(t, "admin", "pw", 30*24*time.Hour)
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
