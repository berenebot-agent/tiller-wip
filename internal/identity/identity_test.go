package identity

import (
	"context"
	"database/sql"
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
