package store_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/crypto"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

func testCipher(t *testing.T, b byte) *crypto.Cipher {
	t.Helper()
	c, err := crypto.New(bytes.Repeat([]byte{b}, crypto.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func openStoreWithCipher(t *testing.T, c store.SecretCipher) (*database.DB, *store.Store) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, store.New(db.SQL, store.WithCipher(c))
}

func createTestProvider(t *testing.T, sc *store.Scope, id, credential string) {
	t.Helper()
	err := sc.CreateProvider(context.Background(), store.CreateProviderInput{
		ID: id, Name: "prov-" + id, Type: "generic-openai",
		BaseURL: "https://example.com", Credential: credential, Enabled: true, Protocols: `[]`,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSecretsAreEncryptedAtRest(t *testing.T) {
	db, st := openStoreWithCipher(t, testCipher(t, 1))
	ctx := context.Background()
	sc := st.For(database.LocalAccountID)
	createTestProvider(t, sc, "p1", "sk-live-secret")

	var raw string
	if err := db.SQL.QueryRowContext(ctx, `SELECT credential_secret FROM providers WHERE id='p1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !crypto.Encrypted(raw) {
		t.Fatalf("stored credential is not encrypted: %q", raw)
	}
	if strings.Contains(raw, "sk-live-secret") {
		t.Fatal("plaintext credential is present in the database")
	}
	load, err := sc.LoadProvider(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if load.Credential != "sk-live-secret" {
		t.Fatalf("LoadProvider credential = %q, want sk-live-secret", load.Credential)
	}

	if err := sc.SetSetting(ctx, store.SettingNotificationsAuthHeader, "Bearer tok"); err != nil {
		t.Fatal(err)
	}
	var rawHeader string
	if err := db.SQL.QueryRowContext(ctx, `SELECT value FROM settings WHERE account_id=? AND key=?`, database.LocalAccountID, store.SettingNotificationsAuthHeader).Scan(&rawHeader); err != nil {
		t.Fatal(err)
	}
	if !crypto.Encrypted(rawHeader) {
		t.Fatalf("stored auth header is not encrypted: %q", rawHeader)
	}
	ns, err := sc.GetNotificationSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ns.AuthHeader != "Bearer tok" {
		t.Fatalf("auth header = %q, want Bearer tok", ns.AuthHeader)
	}

	if err := sc.PutOAuthToken(ctx, store.OAuthTokenRow{
		ProviderID: "p1", AccessToken: "access-tok", RefreshToken: "refresh-tok",
		IDToken: "id-tok", ProviderData: map[string]any{"copilot_token": "copilot-secret"},
	}); err != nil {
		t.Fatal(err)
	}
	var rawAccess, rawData string
	if err := db.SQL.QueryRowContext(ctx, `SELECT access_token,coalesce(provider_data,'') FROM provider_oauth_tokens WHERE provider_id='p1'`).Scan(&rawAccess, &rawData); err != nil {
		t.Fatal(err)
	}
	if !crypto.Encrypted(rawAccess) || !crypto.Encrypted(rawData) {
		t.Fatalf("oauth token not encrypted: access=%q data=%q", rawAccess, rawData)
	}
	tok, err := sc.GetOAuthToken(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-tok" || tok.RefreshToken != "refresh-tok" || tok.IDToken != "id-tok" {
		t.Fatalf("oauth round trip failed: %+v", tok)
	}
	if tok.ProviderData["copilot_token"] != "copilot-secret" {
		t.Fatalf("provider_data round trip failed: %+v", tok.ProviderData)
	}
}

func TestLockedCipherRefusesSecrets(t *testing.T) {
	db, st := openStoreWithCipher(t, testCipher(t, 1))
	ctx := context.Background()
	createTestProvider(t, st.For(database.LocalAccountID), "p1", "sk-live-secret")

	locked := store.New(db.SQL, store.WithCipher(crypto.Locked())).For(database.LocalAccountID)
	if _, err := locked.LoadProvider(ctx, "p1"); !errors.Is(err, store.ErrSecretsLocked) {
		t.Fatalf("LoadProvider under locked cipher = %v, want ErrSecretsLocked", err)
	}
	if _, err := locked.ReplaceProviderCredential(ctx, "p1", "new"); !errors.Is(err, store.ErrSecretsLocked) {
		t.Fatalf("ReplaceProviderCredential under locked cipher = %v, want ErrSecretsLocked", err)
	}
	if err := locked.SetSetting(ctx, store.SettingNotificationsAuthHeader, "Bearer x"); !errors.Is(err, store.ErrSecretsLocked) {
		t.Fatalf("SetSetting under locked cipher = %v, want ErrSecretsLocked", err)
	}
}

func TestMigrateSecretsEncryptsPlaintext(t *testing.T) {
	db, st := openStore(t)
	ctx := context.Background()
	createTestProvider(t, st.For(database.LocalAccountID), "p1", "sk-plain")

	var raw string
	if err := db.SQL.QueryRowContext(ctx, `SELECT credential_secret FROM providers WHERE id='p1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if crypto.Encrypted(raw) {
		t.Fatalf("credential unexpectedly encrypted before migration: %q", raw)
	}
	migrated, locked, err := store.MigrateSecrets(ctx, db.SQL, testCipher(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	if locked {
		t.Fatal("migration reported locked with a valid key")
	}
	if migrated != 1 {
		t.Fatalf("migrated = %d, want 1", migrated)
	}
	if err := db.SQL.QueryRowContext(ctx, `SELECT credential_secret FROM providers WHERE id='p1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !crypto.Encrypted(raw) {
		t.Fatalf("credential not encrypted after migration: %q", raw)
	}
	migrated, locked, err = store.MigrateSecrets(ctx, db.SQL, testCipher(t, 1))
	if err != nil || locked || migrated != 0 {
		t.Fatalf("second migration = (%d,%v,%v), want idempotent", migrated, locked, err)
	}
}

func TestMigrateSecretsLockedOnWrongKey(t *testing.T) {
	db, _ := openStore(t)
	ctx := context.Background()
	createTestProvider(t, store.New(db.SQL, store.WithCipher(testCipher(t, 1))).For(database.LocalAccountID), "p1", "sk-secret")

	migrated, locked, err := store.MigrateSecrets(ctx, db.SQL, testCipher(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("migration with the wrong key did not report locked")
	}
	if migrated != 0 {
		t.Fatalf("migrated = %d with a wrong key, want 0", migrated)
	}
	// The original key still reads the value.
	load, err := store.New(db.SQL, store.WithCipher(testCipher(t, 1))).For(database.LocalAccountID).LoadProvider(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if load.Credential != "sk-secret" {
		t.Fatalf("credential = %q, want sk-secret", load.Credential)
	}
}

// TestRotateSecretsCoversMailOutbox proves key rotation re-encrypts a queued
// outbox one-time token, which uses a distinct AAD from tenant secrets.
func TestRotateSecretsCoversMailOutbox(t *testing.T) {
	db, _ := openStore(t)
	ctx := context.Background()
	cipherA := testCipher(t, 1)
	cipherB := testCipher(t, 2)
	sealed, err := cipherA.Encrypt(crypto.AAD("tiller", "mailoutbox", "row-1", "token"), "raw-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO mail_outbox(id,type,recipient,params,token_ciphertext,attempts,next_attempt_at,created_at) VALUES('row-1','verify_email','a@example.com','{}',?,0,'2030-01-01T00:00:00Z','2030-01-01T00:00:00Z')`, sealed); err != nil {
		t.Fatal(err)
	}
	rotated, err := store.RotateSecrets(ctx, db.SQL, cipherA, cipherB)
	if err != nil {
		t.Fatal(err)
	}
	if rotated != 1 {
		t.Fatalf("rotated = %d, want 1", rotated)
	}
	var stored string
	if err := db.SQL.QueryRow(`SELECT token_ciphertext FROM mail_outbox WHERE id='row-1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	plain, err := cipherB.Decrypt(crypto.AAD("tiller", "mailoutbox", "row-1", "token"), stored)
	if err != nil || plain != "raw-token" {
		t.Fatalf("rotated outbox token = %q, err = %v", plain, err)
	}
	if _, err := cipherA.Decrypt(crypto.AAD("tiller", "mailoutbox", "row-1", "token"), stored); err == nil {
		t.Fatal("old key still decrypts the rotated outbox token")
	}
}

func TestRotateSecrets(t *testing.T) {
	db, _ := openStore(t)
	ctx := context.Background()
	createTestProvider(t, store.New(db.SQL, store.WithCipher(testCipher(t, 1))).For(database.LocalAccountID), "p1", "sk-rotate")

	rotated, err := store.RotateSecrets(ctx, db.SQL, testCipher(t, 1), testCipher(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	if rotated != 1 {
		t.Fatalf("rotated = %d, want 1", rotated)
	}
	if _, err := store.New(db.SQL, store.WithCipher(testCipher(t, 1))).For(database.LocalAccountID).LoadProvider(ctx, "p1"); !errors.Is(err, store.ErrSecretsLocked) {
		t.Fatalf("old key still reads rotated value: %v", err)
	}
	load, err := store.New(db.SQL, store.WithCipher(testCipher(t, 2))).For(database.LocalAccountID).LoadProvider(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if load.Credential != "sk-rotate" {
		t.Fatalf("credential = %q, want sk-rotate", load.Credential)
	}
	if _, err := store.RotateSecrets(ctx, db.SQL, testCipher(t, 9), testCipher(t, 3)); !errors.Is(err, store.ErrSecretsLocked) {
		t.Fatalf("rotation with a wrong old key = %v, want ErrSecretsLocked", err)
	}
}

func TestAADBindsAccount(t *testing.T) {
	db, _ := openStore(t)
	st := store.New(db.SQL, store.WithCipher(testCipher(t, 1)))
	ctx := context.Background()
	createTestProvider(t, st.For(database.LocalAccountID), "p1", "sk-account-a")

	// Copy the ciphertext to a provider in another account; the AAD bound to
	// the original account must prevent decryption.
	other := st.For(otherAccount)
	createTestProvider(t, other, "p2", "placeholder")
	if _, err := db.SQL.ExecContext(ctx, `UPDATE providers SET credential_secret=(SELECT credential_secret FROM providers WHERE id='p1') WHERE id='p2' AND account_id=?`, otherAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := other.LoadProvider(ctx, "p2"); !errors.Is(err, store.ErrSecretsLocked) {
		t.Fatalf("moved ciphertext decrypted under another account: %v", err)
	}
}

func TestHasEncryptedSecrets(t *testing.T) {
	db, _ := openStore(t)
	ctx := context.Background()
	has, err := store.HasEncryptedSecrets(ctx, db.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("fresh database reports encrypted secrets")
	}
	createTestProvider(t, store.New(db.SQL, store.WithCipher(testCipher(t, 1))).For(database.LocalAccountID), "p1", "sk-secret")
	has, err = store.HasEncryptedSecrets(ctx, db.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("database with an encrypted credential reports none")
	}
}
