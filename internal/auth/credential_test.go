package auth_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// newCredentialDB opens a migrated database for credential tests.
func newCredentialDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fastStore builds a SessionStore with the fast test hasher: no KDF cost, but
// real platform_settings rows and session semantics.
func fastStore(t *testing.T, db *database.DB, username, password string) *auth.SessionStore {
	t.Helper()
	store, err := auth.NewSessionStoreWithHasher(db.SQL, username, password, time.Hour, fastsecret.Hasher{})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestCredentialStoreUnconfigured proves an empty boot writes no credential and
// reports the first-run state, so the setup page is offered.
func TestCredentialStoreUnconfigured(t *testing.T) {
	db := newCredentialDB(t)
	store := fastStore(t, db, "", "")
	if store.CredentialConfigured() {
		t.Fatal("empty boot should not count as configured")
	}
	if store.AdminUsername() != "" {
		t.Fatal("empty boot should store no username")
	}
	if store.VerifyCredential("admin", "anything") {
		t.Fatal("unconfigured store verified a credential")
	}
	var rows int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM platform_settings WHERE key IN ('admin_credential_hash','admin_username')`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("empty boot wrote %d credential rows, want 0", rows)
	}
}

// TestEnvCredentialSyncsAndVerifies covers the env-admin path: boot writes the
// hash and the display username from environment values.
func TestEnvCredentialSyncsAndVerifies(t *testing.T) {
	db := newCredentialDB(t)
	store := fastStore(t, db, "env-admin", "env-password")
	if !store.CredentialConfigured() {
		t.Fatal("env credentials should configure the store")
	}
	if !store.VerifyCredential("env-admin", "env-password") {
		t.Fatal("env credential did not verify")
	}
	if store.VerifyCredential("env-admin", "wrong") {
		t.Fatal("wrong password verified")
	}
	if got := store.AdminUsername(); got != "env-admin" {
		t.Fatalf("AdminUsername = %q, want env-admin", got)
	}
}

// TestEnvCredentialChangeInvalidatesSessions proves the env override remains a
// rotation mechanism: a changed env credential rewrites the hash and revokes
// existing sessions.
func TestEnvCredentialChangeInvalidatesSessions(t *testing.T) {
	db := newCredentialDB(t)
	first := fastStore(t, db, "admin", "old-password")
	session, err := first.Create()
	if err != nil {
		t.Fatal(err)
	}
	second := fastStore(t, db, "admin", "new-password")
	if _, ok := second.Get(session.Token); ok {
		t.Fatal("old session survived an env credential change")
	}
	if !second.VerifyCredential("admin", "new-password") {
		t.Fatal("new env credential did not verify")
	}
	if second.VerifyCredential("admin", "old-password") {
		t.Fatal("old password still verifies after env change")
	}
}

// TestSetCredentialIsOneShot is the first-run claim contract: the write lands
// once, and a second claim can never replace the administrator.
func TestSetCredentialIsOneShot(t *testing.T) {
	db := newCredentialDB(t)
	store := fastStore(t, db, "", "")
	if err := store.SetCredential("wizard-admin", "wizard-password"); err != nil {
		t.Fatal(err)
	}
	if !store.CredentialConfigured() || !store.VerifyCredential("wizard-admin", "wizard-password") {
		t.Fatal("wizard credential not effective after SetCredential")
	}
	if got := store.AdminUsername(); got != "wizard-admin" {
		t.Fatalf("AdminUsername = %q, want wizard-admin", got)
	}
	err := store.SetCredential("intruder", "intruder-password")
	if !errors.Is(err, auth.ErrCredentialAlreadySet) {
		t.Fatalf("second claim err = %v, want ErrCredentialAlreadySet", err)
	}
	if !store.VerifyCredential("wizard-admin", "wizard-password") {
		t.Fatal("second claim overwrote the first credential")
	}
	if store.VerifyCredential("intruder", "intruder-password") {
		t.Fatal("intruder credential was accepted")
	}
}

// TestSetCredentialInvalidatesSessions proves any pre-existing session is
// revoked when the credential is first claimed.
func TestSetCredentialInvalidatesSessions(t *testing.T) {
	db := newCredentialDB(t)
	store := fastStore(t, db, "", "")
	session, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCredential("admin", "wizard-password"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get(session.Token); ok {
		t.Fatal("pre-claim session survived SetCredential")
	}
}

// TestWizardCredentialSurvivesRestart proves the stored credential, not the
// environment, is authoritative: a restart with no env credentials still
// authenticates and reports the stored username.
func TestWizardCredentialSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := fastStore(t, db, "", "")
	if err := store.SetCredential("owner", "wizard-password"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	reopened, err := database.Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again := fastStore(t, reopened, "", "")
	if !again.CredentialConfigured() {
		t.Fatal("stored credential did not survive restart")
	}
	if !again.VerifyCredential("owner", "wizard-password") {
		t.Fatal("stored credential did not verify after restart")
	}
	if got := again.AdminUsername(); got != "owner" {
		t.Fatalf("AdminUsername after restart = %q, want owner", got)
	}
}

// TestPartialCredentialRejected proves boot cannot half-configure: both values
// or neither.
func TestPartialCredentialRejected(t *testing.T) {
	db := newCredentialDB(t)
	if _, err := auth.NewSessionStoreWithHasher(db.SQL, "admin", "", time.Hour, fastsecret.Hasher{}); !errors.Is(err, auth.ErrPartialCredential) {
		t.Fatalf("username-only boot err = %v, want ErrPartialCredential", err)
	}
	if _, err := auth.NewSessionStoreWithHasher(db.SQL, "", "password", time.Hour, fastsecret.Hasher{}); !errors.Is(err, auth.ErrPartialCredential) {
		t.Fatalf("password-only boot err = %v, want ErrPartialCredential", err)
	}
}
