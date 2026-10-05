package identity

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := New(db.SQL, fastsecret.Hasher{}, fastsecret.Hasher{}, fastsecret.Hasher{}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return st, db.SQL
}

func TestSignupVerifySessionAndReset(t *testing.T) {
	st, db := newTestStore(t)
	ctx := context.Background()
	result, err := st.CreateSignup(ctx, " User@Example.COM ", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if result.User.Email != "user@example.com" || result.User.AccountStatus != "pending" {
		t.Fatalf("signup user = %+v", result.User)
	}
	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM email_verification_tokens`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == result.VerificationToken || stored == "" {
		t.Fatal("raw verification token was persisted")
	}
	verified, err := st.ConsumeVerification(ctx, result.VerificationToken)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Verified() || verified.AccountStatus != "active" {
		t.Fatalf("verified user = %+v", verified)
	}
	if _, err := st.ConsumeVerification(ctx, result.VerificationToken); err == nil {
		t.Fatal("verification token was reusable")
	}
	session, err := st.CreateUserSession(ctx, verified)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st.GetUserSession(ctx, session.Token)
	if !ok || got.User.AccountID != verified.AccountID {
		t.Fatalf("session lookup = %+v, %v", got, ok)
	}
	_, resetToken, err := st.IssuePasswordReset(ctx, verified.Email)
	if err != nil {
		t.Fatal(err)
	}
	if resetToken == "" {
		t.Fatal("reset token was empty")
	}
	if _, err := st.ConsumePasswordReset(ctx, resetToken, "new correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetUserSession(ctx, session.Token); ok {
		t.Fatal("password reset did not revoke the session")
	}
}

func TestAccountSuspensionInvalidatesSessions(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	result, err := st.CreateSignup(ctx, "suspend@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.ConsumeVerification(ctx, result.VerificationToken)
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateUserSession(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAccountStatus(ctx, u.AccountID, "suspended"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetUserSession(ctx, session.Token); ok {
		t.Fatal("suspended account session remained valid")
	}
}

func TestUserSessionRejectsMismatchedAccountOwner(t *testing.T) {
	st, db := newTestStore(t)
	ctx := context.Background()
	firstSignup, err := st.CreateSignup(ctx, "first@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	firstUser, err := st.ConsumeVerification(ctx, firstSignup.VerificationToken)
	if err != nil {
		t.Fatal(err)
	}
	firstSession, err := st.CreateUserSession(ctx, firstUser)
	if err != nil {
		t.Fatal(err)
	}
	secondSignup, err := st.CreateSignup(ctx, "second@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	secondUser, err := st.ConsumeVerification(ctx, secondSignup.VerificationToken)
	if err != nil {
		t.Fatal(err)
	}
	selector, _, ok := parseOpaqueToken(firstSession.Token)
	if !ok {
		t.Fatal("new user session token did not parse")
	}
	if _, err := db.Exec(`UPDATE user_sessions SET account_id=? WHERE id=?`, secondUser.AccountID, selector); err != nil {
		t.Fatal(err)
	}
	freshStore, err := New(db, st.passwordHasher, st.tokenHasher, st.credentialHasher, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, valid := freshStore.GetUserSession(ctx, firstSession.Token); valid {
		t.Fatal("session with an account not owned by its user was accepted")
	}
}

func TestPlatformCounts(t *testing.T) {
	st, db := newTestStore(t)
	ctx := context.Background()
	if _, err := st.CreateSignup(ctx, "pending@example.com", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateGoogleSignup(ctx, "active@example.com", "google-subject", SignupAcceptance{TermsUpdatedAt: "2026-09-01T00:00:00Z", PrivacyUpdatedAt: "2026-09-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	suspended, err := st.CreateSignup(ctx, "suspended@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE accounts SET status='suspended' WHERE id=?`, suspended.User.AccountID); err != nil {
		t.Fatal(err)
	}
	counts, err := st.PlatformCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Accounts != 3 || counts.Active != 1 || counts.Pending != 1 || counts.Suspended != 1 || counts.Users != 3 {
		t.Fatalf("platform counts = %+v", counts)
	}
	accountIDs, err := st.PlatformAccountIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accountIDs) != 3 {
		t.Fatalf("platform account IDs = %v, want 3 accounts", accountIDs)
	}
	if accountIDs[0] == database.LocalAccountID || accountIDs[1] == database.LocalAccountID || accountIDs[2] == database.LocalAccountID {
		t.Fatalf("platform account IDs include the local bootstrap account: %v", accountIDs)
	}
}

func TestBootstrapHostedCustomerMigratesLocalAccountOnce(t *testing.T) {
	st, db := newTestStore(t)
	ctx := context.Background()
	outcome, err := st.BootstrapHostedCustomer(ctx, "Owner@Example.COM", "correct horse battery staple", true)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != HostedBootstrapMigrated {
		t.Fatalf("first bootstrap outcome = %v, want HostedBootstrapMigrated", outcome)
	}
	var userID, email, ownerID string
	if err := db.QueryRow(`SELECT u.id,u.email,a.owner_user_id FROM users u JOIN accounts a ON a.id=?`, database.LocalAccountID).Scan(&userID, &email, &ownerID); err != nil {
		t.Fatal(err)
	}
	if userID == "" || email != "owner@example.com" || ownerID != userID {
		t.Fatalf("migrated local account = user=%q email=%q owner=%q", userID, email, ownerID)
	}
	outcome, err = st.BootstrapHostedCustomer(ctx, "other@example.com", "another correct horse battery staple", false)
	if err != nil {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	if outcome != HostedBootstrapSkipped {
		t.Fatalf("repeat bootstrap outcome = %v, want HostedBootstrapSkipped", outcome)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("user count after repeat = %d, want 1", count)
	}
}

func TestBootstrapHostedFreshInstallCreatesNoCustomer(t *testing.T) {
	st, db := newTestStore(t)
	outcome, err := st.BootstrapHostedCustomer(context.Background(), "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	// The outcome is what lets the caller warn that supplied credentials were
	// ignored, which is the only signal an operator gets for this branch.
	if outcome != HostedBootstrapFresh {
		t.Fatalf("fresh hosted outcome = %v, want HostedBootstrapFresh", outcome)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("fresh hosted user count = %d, want 0", count)
	}
}

// The store does NOT refuse supplied credentials, and that is load-bearing: on a
// fresh hosted install the completed-bootstrap marker is inserted by
// database.Open (only under WithHostedMode), which short-circuits this call
// before it is reached. A caller that invokes the store directly therefore DOES
// migrate. The server is what refuses to treat the environment as provisioning
// input on a fresh install, by keying its warning on db.FreshInstall. Pinned here
// so a future "fix" does not relocate that refusal somewhere it would never run.
func TestBootstrapHostedStoreMigratesWhenInvokedDirectly(t *testing.T) {
	st, db := newTestStore(t)
	outcome, err := st.BootstrapHostedCustomer(context.Background(), "owner@example.com", "correct horse battery staple", true)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != HostedBootstrapMigrated {
		t.Fatalf("direct invocation outcome = %v, want HostedBootstrapMigrated "+
			"(the fresh-install refusal lives in the server, keyed on db.FreshInstall)", outcome)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("direct invocation user count = %d, want 1", count)
	}
}

// The production composition: database.Open records the completed bootstrap for
// a fresh hosted install, so the store reports Skipped and creates nothing —
// which is why the server cannot rely on the Fresh outcome to detect it.
func TestBootstrapHostedFreshHostedDatabaseCreatesNoCustomer(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"), database.WithHostedMode(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if !db.FreshInstall {
		t.Fatal("expected a fresh install")
	}
	st, err := New(db.SQL, fastsecret.Hasher{}, fastsecret.Hasher{}, fastsecret.Hasher{}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := st.BootstrapHostedCustomer(context.Background(), "owner@example.com", "correct horse battery staple", db.FreshInstall)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != HostedBootstrapSkipped {
		t.Fatalf("fresh hosted database outcome = %v, want HostedBootstrapSkipped "+
			"(database.Open pre-records the marker)", outcome)
	}
	var count int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("fresh hosted user count = %d, want 0", count)
	}
}

// An existing local database (not a fresh install) with valid credentials
// migrates, and only once.
func TestBootstrapHostedExistingDatabaseMigratesOnce(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := New(db.SQL, fastsecret.Hasher{}, fastsecret.Hasher{}, fastsecret.Hasher{}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := st.BootstrapHostedCustomer(context.Background(), "owner@example.com", "correct horse battery staple", false)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != HostedBootstrapMigrated {
		t.Fatalf("existing-database outcome = %v, want HostedBootstrapMigrated", outcome)
	}
	again, err := st.BootstrapHostedCustomer(context.Background(), "other@example.com", "another correct horse battery staple", false)
	if err != nil {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	if again != HostedBootstrapSkipped {
		t.Fatalf("repeat outcome = %v, want HostedBootstrapSkipped", again)
	}
}

// A database that already booted on the unified local operator model owns
// LocalAccountID. Hosted bootstrap must convert that same user (preserving its
// id, so passkeys survive) rather than inserting a second user that the
// owner_user_id IS NULL gate would reject as a collision.
func TestBootstrapHostedCustomerConvertsExistingLocalOperatorInPlace(t *testing.T) {
	st, db := newTestStore(t)
	ctx := context.Background()
	operator, err := st.EnsureLocalOperator(ctx, "operator", "correct horse battery staple", "")
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := st.BootstrapHostedCustomer(ctx, "owner@example.com", "correct horse battery staple", false)
	if err != nil {
		t.Fatalf("convert local operator: %v", err)
	}
	if outcome != HostedBootstrapMigrated {
		t.Fatalf("outcome = %v, want HostedBootstrapMigrated", outcome)
	}
	var id, email, owner string
	if err := db.QueryRow(`SELECT u.id,u.email,a.owner_user_id FROM users u JOIN accounts a ON a.id=?`, database.LocalAccountID).Scan(&id, &email, &owner); err != nil {
		t.Fatal(err)
	}
	if id != operator.ID {
		t.Fatalf("converted user id = %q, want preserved id %q", id, operator.ID)
	}
	if email != "owner@example.com" || owner != operator.ID {
		t.Fatalf("converted row = id=%q email=%q owner=%q", id, email, owner)
	}
	// The migrated credential must authenticate with hosted bare-password
	// semantics, and no longer with the local username-bound fingerprint.
	if _, err := st.AuthenticatePassword(ctx, "owner@example.com", "correct horse battery staple"); err != nil {
		t.Fatalf("hosted password did not authenticate after conversion: %v", err)
	}
	if _, err := st.AuthenticateLocalOperator(ctx, "operator", "correct horse battery staple"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("local operator login still works after conversion: %v", err)
	}
}

func TestBootstrapHostedInvalidCredentialsDoNotMutate(t *testing.T) {
	st, db := newTestStore(t)
	if _, err := st.BootstrapHostedCustomer(context.Background(), "not-an-email", "short", false); !errors.Is(err, ErrBootstrapInvalid) {
		t.Fatalf("invalid bootstrap error = %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid bootstrap user count = %d, want 0", count)
	}
}
