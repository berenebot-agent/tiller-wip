package identity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
)

func TestEnsureLocalOperatorCreatesAndAttachesUser(t *testing.T) {
	st, db := newTestStore(t)
	ctx := context.Background()

	u, err := st.EnsureLocalOperator(ctx, "operator", "correct horse battery staple", "")
	if err != nil {
		t.Fatal(err)
	}
	if u.AccountID != database.LocalAccountID {
		t.Fatalf("operator account = %q, want LocalAccountID", u.AccountID)
	}
	if u.Email != LocalOperatorEmail("operator") {
		t.Fatalf("operator email = %q, want synthetic local email", u.Email)
	}
	if !u.Verified() || u.AccountStatus != "active" || !u.PasswordEnabled {
		t.Fatalf("operator row = %+v, want verified/active/password", u)
	}

	var owner string
	if err := db.QueryRow(`SELECT owner_user_id FROM accounts WHERE id=?`, database.LocalAccountID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != u.ID {
		t.Fatalf("owner_user_id = %q, want operator %q", owner, u.ID)
	}

	got, err := st.LocalOperatorUserID(ctx)
	if err != nil || got != u.ID {
		t.Fatalf("LocalOperatorUserID = %q, %v; want %q", got, err, u.ID)
	}
}

func TestEnsureLocalOperatorIsIdempotent(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	first, err := st.EnsureLocalOperator(ctx, "operator", "correct horse battery staple", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureLocalOperator(ctx, "operator", "different password entirely", ""); !errors.Is(err, ErrLocalOperatorExists) {
		t.Fatalf("second EnsureLocalOperator err = %v, want ErrLocalOperatorExists", err)
	}
	// The first operator must be untouched.
	still, err := st.LocalOperatorUserID(ctx)
	if err != nil || still != first.ID {
		t.Fatalf("operator id changed: %q, %v; want %q", still, err, first.ID)
	}
}

func TestEnsureLocalOperatorRejectsEmptyUsername(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := st.EnsureLocalOperator(context.Background(), "  ", "correct horse battery staple", ""); !errors.Is(err, ErrBootstrapInvalid) {
		t.Fatalf("empty username err = %v, want ErrBootstrapInvalid", err)
	}
}

func TestEnsureLocalOperatorUsesProvidedHash(t *testing.T) {
	st, db := newTestStore(t)
	ctx := context.Background()

	const preHashed = "$argon2id$precomputed-credential-hash"
	u, err := st.EnsureLocalOperator(ctx, "operator", "", preHashed)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRow(`SELECT password_hash FROM users WHERE id=?`, u.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != preHashed {
		t.Fatalf("stored hash = %q, want the migrated credential hash", stored)
	}
}

func TestAuthenticateLocalOperator(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := st.EnsureLocalOperator(ctx, "operator", "correct horse battery staple", ""); err != nil {
		t.Fatal(err)
	}

	u, err := st.AuthenticateLocalOperator(ctx, "operator", "correct horse battery staple")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if u.Email != LocalOperatorEmail("operator") {
		t.Fatalf("authenticated email = %q", u.Email)
	}
	if _, err := st.AuthenticateLocalOperator(ctx, "operator", "wrong password value"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong password err = %v, want ErrNotFound", err)
	}
	// A correct password under the wrong username must still fail.
	if _, err := st.AuthenticateLocalOperator(ctx, "someoneelse", "correct horse battery staple"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong username err = %v, want ErrNotFound", err)
	}
}

func TestAuthenticateLocalOperatorWithoutRow(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := st.AuthenticateLocalOperator(context.Background(), "operator", "correct horse battery staple"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSyncLocalOperatorCredentialsUpdatesUsernameAndPassword(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	first, err := st.EnsureLocalOperator(ctx, "operator", "correct horse battery staple", "")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the boot-time credential sync after TILLER_USERNAME and
	// TILLER_PASSWORD both changed: the synthetic email must move with the
	// username or AuthenticateLocalOperator rejects the new name before the
	// password is ever checked.
	newHash, err := st.passwordHasher.Hash("new-operator\x00a completely new password")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SyncLocalOperatorCredentials(ctx, "new-operator", newHash); err != nil {
		t.Fatal(err)
	}
	u, err := st.AuthenticateLocalOperator(ctx, "new-operator", "a completely new password")
	if err != nil {
		t.Fatalf("new username/password did not authenticate: %v", err)
	}
	if u.ID != first.ID {
		t.Fatalf("credential sync changed the user id: %q, want %q", u.ID, first.ID)
	}
	if u.Email != LocalOperatorEmail("new-operator") {
		t.Fatalf("synced email = %q, want %q", u.Email, LocalOperatorEmail("new-operator"))
	}
	if _, err := st.AuthenticateLocalOperator(ctx, "operator", "correct horse battery staple"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old username/password still authenticates: %v", err)
	}
	// A no-op sync must not churn the auth generation, so an unchanged boot
	// leaves live sessions alone.
	if err := st.SyncLocalOperatorCredentials(ctx, "new-operator", newHash); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := st.db.QueryRow(`SELECT auth_generation FROM users WHERE id=?`, first.ID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != u.AuthGeneration {
		t.Fatalf("no-op sync bumped generation: got %d, want %d", generation, u.AuthGeneration)
	}
}

func TestSyncLocalOperatorCredentialsNoRowIsNoop(t *testing.T) {
	st, _ := newTestStore(t)
	// No operator row exists yet; the boot sync must not error (the row is
	// materialised later by EnsureLocalOperator).
	if err := st.SyncLocalOperatorCredentials(context.Background(), "operator", "some-hash"); err != nil {
		t.Fatalf("sync without a row = %v, want nil", err)
	}
}

func TestLocalOperatorEmailNormalizesUsername(t *testing.T) {
	if got := LocalOperatorEmail("  Operator  "); !strings.HasSuffix(got, "@local.invalid") || got != "operator@local.invalid" {
		t.Fatalf("LocalOperatorEmail = %q", got)
	}
}
