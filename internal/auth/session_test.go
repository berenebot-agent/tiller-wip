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

// TestSessionStoreProductionHasher verifies the production tiered path:
// high-entropy session tokens are bcrypt-hashed, while the low-entropy admin
// credential fingerprint keeps the memory-hard Argon2id KDF (64MiB/3/4).
func TestSessionStoreProductionHasher(t *testing.T) {
	store, _ := newTestStore(t, "admin", "pw", 30*24*time.Hour)
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_, secret, _ := parseSessionToken(session.Token)
	var tokenHash string
	if err := store.db.QueryRow(`SELECT token_hash FROM admin_sessions`).Scan(&tokenHash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tokenHash, "$2") {
		t.Fatalf("expected bcrypt session token hash, got %q", tokenHash)
	}
	if !store.tokenHasher.Verify(secret, tokenHash) {
		t.Fatal("correct secret did not verify against production session hash")
	}
	if store.tokenHasher.Verify(secret+"wrong", tokenHash) {
		t.Fatal("incorrect secret verified against production session hash")
	}
	if store.tokenHasher.Verify(secret, "$malformed$hash") {
		t.Fatal("malformed hash verified")
	}
	if tokenHash == secret {
		t.Fatal("raw session secret stored in database")
	}

	// The admin credential fingerprint remains Argon2id.
	var credentialHash string
	if err := store.db.QueryRow(`SELECT value FROM platform_settings WHERE key=?`, credentialHashKey).Scan(&credentialHash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(credentialHash, "$argon2id$") {
		t.Fatalf("expected argon2id credential fingerprint, got %q", credentialHash)
	}
	memory, iterations, lanes, err := ArgonParameters(credentialHash)
	if err != nil {
		t.Fatal(err)
	}
	if memory != 64*1024 || iterations != 3 || lanes != 4 {
		t.Fatalf("unexpected Argon2id parameters: %d/%d/%d", memory, iterations, lanes)
	}
	if !store.credentialHasher.Verify("admin\x00pw", credentialHash) {
		t.Fatal("credential fingerprint did not verify")
	}
}
