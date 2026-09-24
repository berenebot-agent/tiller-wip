package auth

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

func TestBcryptHasherRoundTrip(t *testing.T) {
	h := BcryptHasher{Cost: 4}
	encoded, err := h.Hash("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$2") {
		t.Fatalf("expected bcrypt encoding, got %q", encoded)
	}
	if !h.Verify("s3cret", encoded) {
		t.Fatal("correct secret failed bcrypt verify")
	}
	if h.Verify("wrong", encoded) {
		t.Fatal("wrong secret passed bcrypt verify")
	}
	if h.NeedsRehash(encoded) {
		t.Fatal("current-cost bcrypt hash was flagged for rehash")
	}
	if !h.NeedsRehash("$argon2id$v=19$m=65536,t=3,p=4$abc$def") {
		t.Fatal("legacy argon2 encoding should need rehash")
	}
	if !h.NeedsRehash("garbage") {
		t.Fatal("non-bcrypt encoding should need rehash")
	}
}

func TestBcryptNeedsRehashOnCostChange(t *testing.T) {
	encoded, err := (BcryptHasher{Cost: 4}).Hash("s")
	if err != nil {
		t.Fatal(err)
	}
	if !(BcryptHasher{Cost: 5}).NeedsRehash(encoded) {
		t.Fatal("lower-cost hash should rehash under a higher target cost")
	}
	if (BcryptHasher{Cost: 4}).NeedsRehash(encoded) {
		t.Fatal("same-cost hash should not rehash")
	}
}

func TestVerifyEncodedAcceptsLegacyArgon2(t *testing.T) {
	legacy, err := argon2idHash("legacy-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyEncoded("legacy-secret", legacy) {
		t.Fatal("legacy argon2 secret did not verify")
	}
	if VerifyEncoded("wrong", legacy) {
		t.Fatal("wrong secret verified against legacy argon2 hash")
	}
	if VerifyEncoded("legacy-secret", "$unknown$hash") {
		t.Fatal("unknown encoding prefix verified")
	}
	if !(BcryptHasher{Cost: 4}).Verify("legacy-secret", legacy) {
		t.Fatal("token hasher must accept legacy argon2 rows")
	}
}

// TestClientAuthenticatorLazyRehash stores a client key using the legacy
// argon2id format, authenticates it, and asserts the row is transparently
// upgraded to bcrypt.
func TestClientAuthenticatorLazyRehash(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	generated, err := GenerateKeyWithHasher(Argon2Hasher{})
	if err != nil {
		t.Fatal(err)
	}
	now := database.Now()
	if _, err := db.SQL.Exec(`INSERT INTO client_keys(id,name,selector,secret_hash,secret_fingerprint,enabled,created_at,updated_at) VALUES(?,?,?,?,?,1,?,?)`,
		"client-1", "probe", generated.Selector, generated.Hash, generated.Fingerprint, now, now); err != nil {
		t.Fatal(err)
	}

	a, err := NewClientAuthenticatorWithHasher(db.SQL, BcryptHasher{Cost: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Authenticate(generated.Plaintext); !ok {
		t.Fatal("legacy argon2 client key did not authenticate")
	}
	var stored string
	if err := db.SQL.QueryRow(`SELECT secret_hash FROM client_keys WHERE id=?`, "client-1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "$2") {
		t.Fatalf("expected lazy rehash to bcrypt, got %q", stored)
	}
}

// TestSessionStoreLegacyTokenMigration creates a session under the legacy
// argon2id token hasher and asserts a bcrypt-configured store validates it and
// upgrades the stored hash.
func TestSessionStoreLegacyTokenMigration(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	legacy, err := NewSessionStoreTiered(db.SQL, "admin", "pw", time.Hour, Argon2Hasher{}, BcryptHasher{Cost: 4})
	if err != nil {
		t.Fatal(err)
	}
	session, err := legacy.Create()
	if err != nil {
		t.Fatal(err)
	}
	var before string
	if err := db.SQL.QueryRow(`SELECT token_hash FROM admin_sessions`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(before, "$argon2id$") {
		t.Fatalf("expected legacy argon2 session hash, got %q", before)
	}

	upgraded, err := NewSessionStoreTiered(db.SQL, "admin", "pw", time.Hour, BcryptHasher{Cost: 4}, BcryptHasher{Cost: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := upgraded.Get(session.Token); !ok {
		t.Fatal("legacy argon2 session did not validate")
	}
	var after string
	if err := db.SQL.QueryRow(`SELECT token_hash FROM admin_sessions`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(after, "$2") {
		t.Fatalf("expected lazy rehash to bcrypt, got %q", after)
	}
}
