package auth

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

func insertManagementToken(t *testing.T, db *sql.DB, id string, g GeneratedKey, scopes, expiresAt string) {
	t.Helper()
	now := database.Now()
	var expires any
	if expiresAt != "" {
		expires = expiresAt
	}
	if _, err := db.Exec(`INSERT INTO api_tokens(id,name,selector,secret_hash,secret_fingerprint,scopes,enabled,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,1,?,?,?)`,
		id, id, g.Selector, g.Hash, g.Fingerprint, scopes, expires, now, now); err != nil {
		t.Fatal(err)
	}
}

func TestManagementTokenPrefixSeparation(t *testing.T) {
	hasher := &countingHasher{}
	generated, err := GenerateManagementTokenWithHasher(hasher)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(generated.Plaintext, "mt-tr-") {
		t.Fatalf("unexpected prefix: %s", generated.Plaintext)
	}
	selector, secret, ok := ParseManagementToken(generated.Plaintext)
	if !ok || selector != generated.Selector || len(secret) != 43 {
		t.Fatal("generated management token did not parse")
	}
	// A management token must never parse as a client key, and vice versa.
	if _, _, ok := ParseKey(generated.Plaintext); ok {
		t.Fatal("client-key parser accepted a management token")
	}
	clientKey, err := GenerateKeyWithHasher(hasher)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ParseManagementToken(clientKey.Plaintext); ok {
		t.Fatal("management-token parser accepted a client key")
	}
	for _, value := range []string{"", "mt-tr-a.b", "mt-tr-" + clientKey.Plaintext, "sk-tr-selector.secret"} {
		if _, _, ok := ParseManagementToken(value); ok {
			t.Fatalf("accepted malformed management token %q", value)
		}
	}
}

func TestValidateManagementScopes(t *testing.T) {
	encoded, err := ValidateManagementScopes(map[string]int{"providers": ScopeWrite, "monitoring": ScopeRead})
	if err != nil {
		t.Fatal(err)
	}
	if encoded != `{"monitoring":1,"providers":2}` {
		t.Fatalf("canonical scopes = %s", encoded)
	}
	if _, err := ValidateManagementScopes(map[string]int{"bogus": ScopeRead}); err == nil {
		t.Fatal("unknown scope area accepted")
	}
	for _, tier := range []int{0, 4, -1} {
		if _, err := ValidateManagementScopes(map[string]int{"providers": tier}); err == nil {
			t.Fatalf("out-of-range tier %d accepted", tier)
		}
	}
	if encoded, err := ValidateManagementScopes(nil); err != nil || encoded != `{}` {
		t.Fatalf("empty scopes = %q, %v", encoded, err)
	}
}

func TestParseManagementScopesFailsClosed(t *testing.T) {
	if got := ParseManagementScopes(`not json`); len(got) != 0 {
		t.Fatalf("malformed scopes did not fail closed: %v", got)
	}
	if got := ParseManagementScopes(``); len(got) != 0 {
		t.Fatalf("empty scopes = %v", got)
	}
	got := ParseManagementScopes(`{"providers":3}`)
	if got["providers"] != ScopeDelete {
		t.Fatalf("scopes = %v", got)
	}
}

func TestManagementTokenAuthenticatorLifecycle(t *testing.T) {
	db := openAuthTestDB(t)
	hasher := &countingHasher{}
	authenticator, err := NewManagementTokenAuthenticatorWithHasher(db.SQL, hasher)
	if err != nil {
		t.Fatal(err)
	}
	authenticator.SetCacheTTL(time.Minute)
	generated, err := GenerateManagementTokenWithHasher(hasher)
	if err != nil {
		t.Fatal(err)
	}
	insertManagementToken(t, db.SQL, "tok-1", generated, `{"providers":2}`, "")

	ctx := context.Background()
	identity, err := authenticator.AuthenticateContext(ctx, generated.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != "tok-1" || identity.AccountID != database.LocalAccountID {
		t.Fatalf("identity = %+v", identity)
	}
	if !identity.Allows("providers", ScopeRead) || !identity.Allows("providers", ScopeWrite) {
		t.Fatal("write ceiling did not satisfy read/write")
	}
	if identity.Allows("providers", ScopeDelete) {
		t.Fatal("write ceiling satisfied delete")
	}
	if identity.Allows("virtual", ScopeRead) {
		t.Fatal("grant leaked across areas")
	}

	// A second verification must be served by the cache.
	verifies := hasher.verifies.Load()
	if _, err := authenticator.AuthenticateContext(ctx, generated.Plaintext); err != nil {
		t.Fatal(err)
	}
	if hasher.verifies.Load() != verifies {
		t.Fatal("cache hit re-verified the secret")
	}
	// Invalidation must force a fresh verification.
	authenticator.Invalidate("tok-1")
	if _, err := authenticator.AuthenticateContext(ctx, generated.Plaintext); err != nil {
		t.Fatal(err)
	}
	if hasher.verifies.Load() == verifies {
		t.Fatal("invalidated token was served from cache")
	}

	// A well-formed token that does not exist is invalid.
	other, err := GenerateManagementTokenWithHasher(hasher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.AuthenticateContext(ctx, other.Plaintext); err == nil {
		t.Fatal("unknown selector authenticated")
	} else if err != ErrManagementTokenInvalid {
		t.Fatalf("unknown selector error = %v", err)
	}

	// A client key must never authenticate the management surface.
	clientKey, err := GenerateKeyWithHasher(hasher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.AuthenticateContext(ctx, clientKey.Plaintext); err != ErrManagementTokenInvalid {
		t.Fatalf("client key on management surface error = %v", err)
	}
}

func TestManagementTokenAuthenticatorExpiry(t *testing.T) {
	db := openAuthTestDB(t)
	hasher := &countingHasher{}
	authenticator, err := NewManagementTokenAuthenticatorWithHasher(db.SQL, hasher)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := GenerateManagementTokenWithHasher(hasher)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	insertManagementToken(t, db.SQL, "tok-expired", generated, `{"providers":1}`, past)

	_, err = authenticator.AuthenticateContext(context.Background(), generated.Plaintext)
	if err != ErrManagementTokenExpired {
		t.Fatalf("expired token error = %v", err)
	}

	// A wrong secret on an expired row reports invalid, not expired: the
	// expiry is only disclosed to a holder of the secret.
	wrong := generated.Plaintext[:len(generated.Plaintext)-1]
	if generated.Plaintext[len(generated.Plaintext)-1] == 'A' {
		wrong += "B"
	} else {
		wrong += "A"
	}
	if _, err := authenticator.AuthenticateContext(context.Background(), wrong); err != ErrManagementTokenInvalid {
		t.Fatalf("wrong secret error = %v", err)
	}
}

func TestManagementTokenCacheEntryExpiresWhileCached(t *testing.T) {
	db := openAuthTestDB(t)
	hasher := &countingHasher{}
	authenticator, err := NewManagementTokenAuthenticatorWithHasher(db.SQL, hasher)
	if err != nil {
		t.Fatal(err)
	}
	authenticator.SetCacheTTL(time.Minute)
	generated, err := GenerateManagementTokenWithHasher(hasher)
	if err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(40 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	insertManagementToken(t, db.SQL, "tok-soon", generated, `{}`, soon)

	ctx := context.Background()
	if _, err := authenticator.AuthenticateContext(ctx, generated.Plaintext); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := authenticator.AuthenticateContext(ctx, generated.Plaintext); err != ErrManagementTokenExpired {
		t.Fatalf("cached token past expiry = %v", err)
	}
}

func TestManagementTokenInvalidateWithPublishesUnderLock(t *testing.T) {
	db := openAuthTestDB(t)
	hasher := &countingHasher{}
	authenticator, err := NewManagementTokenAuthenticatorWithHasher(db.SQL, hasher)
	if err != nil {
		t.Fatal(err)
	}
	authenticator.SetCacheTTL(time.Minute)
	generated, err := GenerateManagementTokenWithHasher(hasher)
	if err != nil {
		t.Fatal(err)
	}
	insertManagementToken(t, db.SQL, "tok-2", generated, `{"providers":3}`, "")
	ctx := context.Background()
	if _, err := authenticator.AuthenticateContext(ctx, generated.Plaintext); err != nil {
		t.Fatal(err)
	}
	err = authenticator.InvalidateWith("tok-2", func() error {
		var scopes string
		if err := db.SQL.QueryRow(`SELECT scopes FROM api_tokens WHERE id='tok-2'`).Scan(&scopes); err != nil {
			return err
		}
		if scopes != `{"providers":3}` {
			t.Fatalf("publish observed scopes %q", scopes)
		}
		_, err := db.SQL.Exec(`UPDATE api_tokens SET scopes='{"providers":1}' WHERE id='tok-2'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := authenticator.AuthenticateContext(ctx, generated.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Allows("providers", ScopeWrite) {
		t.Fatal("stale scope served after InvalidateWith")
	}
}
