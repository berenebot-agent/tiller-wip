package auth

import (
	"context"
	"path/filepath"
	"strings"
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

// TestSessionStoreProductionHasher verifies the production Argon2id path: hashes
// use the argon2id PHC prefix, parameters are 64MiB/3/4, correct/incorrect
// secrets verify/fail, malformed PHC fails, and material is not stored
// plaintext.
func TestSessionStoreProductionHasher(t *testing.T) {
	store, db := newTestStore(t, "admin", "pw", 30*24*time.Hour)
	_ = db
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_, secret, _ := parseSessionToken(session.Token)
	var tokenHash string
	if err := store.db.QueryRow(`SELECT token_hash FROM admin_sessions`).Scan(&tokenHash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tokenHash, "$argon2id$") {
		t.Fatalf("expected argon2id hash, got %q", tokenHash)
	}
	memory, iterations, lanes, err := ArgonParameters(tokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if memory != 64*1024 || iterations != 3 || lanes != 4 {
		t.Fatalf("unexpected Argon2id parameters: %d/%d/%d", memory, iterations, lanes)
	}
	if !store.hasher.Verify(secret, tokenHash) {
		t.Fatal("correct secret did not verify against production hash")
	}
	if store.hasher.Verify(secret+"wrong", tokenHash) {
		t.Fatal("incorrect secret verified against production hash")
	}
	if store.hasher.Verify(secret, "$malformed$hash") {
		t.Fatal("malformed PHC verified")
	}
	if tokenHash == secret {
		t.Fatal("raw session secret stored in database")
	}
}
