package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

const countingPrefix = "test-count:"

// countingHasher is a fast, deterministic SecretHasher that records how many
// Hash/Verify calls happen, so tests can prove cache hits skip verification.
type countingHasher struct {
	hashes   atomic.Int64
	verifies atomic.Int64
}

func (h *countingHasher) Hash(secret string) (string, error) {
	h.hashes.Add(1)
	sum := sha256.Sum256([]byte("count\x00" + secret))
	return countingPrefix + hex.EncodeToString(sum[:]), nil
}

func (h *countingHasher) Verify(secret, encoded string) bool {
	h.verifies.Add(1)
	if !strings.HasPrefix(encoded, countingPrefix) {
		return false
	}
	sum := sha256.Sum256([]byte("count\x00" + secret))
	want, err := hex.DecodeString(encoded[len(countingPrefix):])
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(want, sum[:]) == 1
}

func (h *countingHasher) NeedsRehash(string) bool { return false }

var _ SecretHasher = (*countingHasher)(nil)

func openAuthTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertClientKey(t *testing.T, db *sql.DB, id, name string, g GeneratedKey) {
	t.Helper()
	now := database.Now()
	if _, err := db.Exec(`INSERT INTO client_keys(id,name,selector,secret_hash,secret_fingerprint,enabled,created_at,updated_at) VALUES(?,?,?,?,?,1,?,?)`,
		id, name, g.Selector, g.Hash, g.Fingerprint, now, now); err != nil {
		t.Fatal(err)
	}
}

func TestClientKeyCacheHitSkipsVerification(t *testing.T) {
	db := openAuthTestDB(t)
	h := &countingHasher{}
	gen, err := GenerateKeyWithHasher(h)
	if err != nil {
		t.Fatal(err)
	}
	insertClientKey(t, db.SQL, "ck-1", "k1", gen)
	a, err := NewClientAuthenticatorWithHasher(db.SQL, h)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Authenticate(gen.Plaintext); !ok {
		t.Fatal("valid key did not authenticate")
	}
	if got := h.verifies.Load(); got != 1 {
		t.Fatalf("verifies after first auth = %d, want 1", got)
	}
	if _, ok := a.Authenticate(gen.Plaintext); !ok {
		t.Fatal("cached key did not authenticate")
	}
	if got := h.verifies.Load(); got != 1 {
		t.Fatalf("cache hit re-verified: verifies = %d, want 1", got)
	}
}

func TestClientKeyCacheSlidingRenewal(t *testing.T) {
	db := openAuthTestDB(t)
	h := &countingHasher{}
	gen, err := GenerateKeyWithHasher(h)
	if err != nil {
		t.Fatal(err)
	}
	insertClientKey(t, db.SQL, "ck-slide", "k-slide", gen)
	a, err := NewClientAuthenticatorWithHasher(db.SQL, h)
	if err != nil {
		t.Fatal(err)
	}
	a.SetCacheTTL(250 * time.Millisecond)
	if _, ok := a.Authenticate(gen.Plaintext); !ok {
		t.Fatal("valid key did not authenticate")
	}
	// Keep hitting the key within the window; sliding renewal should keep it
	// warm well past the original 250ms TTL.
	for i := 0; i < 5; i++ {
		time.Sleep(150 * time.Millisecond)
		if _, ok := a.Authenticate(gen.Plaintext); !ok {
			t.Fatal("renewed cache entry lost authentication")
		}
	}
	if got := h.verifies.Load(); got != 1 {
		t.Fatalf("sliding renewal failed: verifies = %d, want 1", got)
	}
}

func TestClientKeyCacheExpiryAndSweep(t *testing.T) {
	db := openAuthTestDB(t)
	h := &countingHasher{}
	gen, err := GenerateKeyWithHasher(h)
	if err != nil {
		t.Fatal(err)
	}
	insertClientKey(t, db.SQL, "ck-exp", "k-exp", gen)
	a, err := NewClientAuthenticatorWithHasher(db.SQL, h)
	if err != nil {
		t.Fatal(err)
	}
	a.SetCacheTTL(30 * time.Millisecond)
	if _, ok := a.Authenticate(gen.Plaintext); !ok {
		t.Fatal("valid key did not authenticate")
	}
	time.Sleep(60 * time.Millisecond)
	a.SweepExpired()
	a.mu.Lock()
	remaining := len(a.entries)
	a.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("sweep left %d expired entries, want 0", remaining)
	}
	if _, ok := a.Authenticate(gen.Plaintext); !ok {
		t.Fatal("expired key did not re-authenticate")
	}
	if got := h.verifies.Load(); got != 2 {
		t.Fatalf("verifies after expiry = %d, want 2", got)
	}
}

func TestClientKeyCacheCapEviction(t *testing.T) {
	db := openAuthTestDB(t)
	h := &countingHasher{}
	a, err := NewClientAuthenticatorWithHasher(db.SQL, h)
	if err != nil {
		t.Fatal(err)
	}
	a.maxEntries = 2
	for i, id := range []string{"cap-1", "cap-2", "cap-3"} {
		gen, err := GenerateKeyWithHasher(h)
		if err != nil {
			t.Fatal(err)
		}
		insertClientKey(t, db.SQL, id, "name-"+id, gen)
		if _, ok := a.Authenticate(gen.Plaintext); !ok {
			t.Fatalf("key %d did not authenticate", i)
		}
	}
	a.mu.Lock()
	n := len(a.entries)
	a.mu.Unlock()
	if n > 2 {
		t.Fatalf("cache size = %d, want <= 2", n)
	}
}

func TestSessionCacheSlidingRenewal(t *testing.T) {
	db := openAuthTestDB(t)
	h := &countingHasher{}
	s, err := NewSessionStoreTiered(db.SQL, "admin", "pw", time.Hour, h, h)
	if err != nil {
		t.Fatal(err)
	}
	s.SetCacheTTL(250 * time.Millisecond)
	session, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	base := h.verifies.Load()
	if _, ok := s.Get(session.Token); !ok {
		t.Fatal("valid session did not validate")
	}
	if got := h.verifies.Load(); got != base+1 {
		t.Fatalf("verifies after first get = %d, want %d", got, base+1)
	}
	for i := 0; i < 5; i++ {
		time.Sleep(150 * time.Millisecond)
		if _, ok := s.Get(session.Token); !ok {
			t.Fatal("renewed session cache entry lost validity")
		}
	}
	if got := h.verifies.Load(); got != base+1 {
		t.Fatalf("session sliding renewal failed: verifies = %d, want %d", got, base+1)
	}
}

func TestSessionCacheSweepAndCap(t *testing.T) {
	db := openAuthTestDB(t)
	h := &countingHasher{}
	s, err := NewSessionStoreTiered(db.SQL, "admin", "pw", time.Hour, h, h)
	if err != nil {
		t.Fatal(err)
	}
	s.SetCacheTTL(30 * time.Millisecond)
	session, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(session.Token); !ok {
		t.Fatal("valid session did not validate")
	}
	time.Sleep(60 * time.Millisecond)
	s.SweepExpired()
	s.mu.Lock()
	n := len(s.cache)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("sweep left %d expired session entries, want 0", n)
	}

	// Cap: with a tiny cap, populating more sessions must not grow past it.
	s.SetCacheTTL(time.Hour)
	s.maxEntries = 2
	for i := 0; i < 3; i++ {
		created, err := s.Create()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := s.Get(created.Token); !ok {
			t.Fatalf("session %d did not validate", i)
		}
	}
	s.mu.Lock()
	n = len(s.cache)
	s.mu.Unlock()
	if n > 2 {
		t.Fatalf("session cache size = %d, want <= 2", n)
	}
}
