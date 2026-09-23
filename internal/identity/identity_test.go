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

func TestBootstrapHostedCustomerMigratesLocalAccountOnce(t *testing.T) {
	st, db := newTestStore(t)
	ctx := context.Background()
	if err := st.BootstrapHostedCustomer(ctx, "Owner@Example.COM", "correct horse battery staple", true); err != nil {
		t.Fatal(err)
	}
	var userID, email, ownerID string
	if err := db.QueryRow(`SELECT u.id,u.email,a.owner_user_id FROM users u JOIN accounts a ON a.id=?`, database.LocalAccountID).Scan(&userID, &email, &ownerID); err != nil {
		t.Fatal(err)
	}
	if userID == "" || email != "owner@example.com" || ownerID != userID {
		t.Fatalf("migrated local account = user=%q email=%q owner=%q", userID, email, ownerID)
	}
	if err := st.BootstrapHostedCustomer(ctx, "other@example.com", "another correct horse battery staple", false); err != nil {
		t.Fatalf("repeat bootstrap: %v", err)
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
	if err := st.BootstrapHostedCustomer(context.Background(), "", "", true); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("fresh hosted user count = %d, want 0", count)
	}
}

func TestBootstrapHostedInvalidCredentialsDoNotMutate(t *testing.T) {
	st, db := newTestStore(t)
	if err := st.BootstrapHostedCustomer(context.Background(), "not-an-email", "short", false); !errors.Is(err, ErrBootstrapInvalid) {
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
