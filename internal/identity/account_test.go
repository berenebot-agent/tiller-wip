package identity

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/mailoutbox"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// recordingQueue captures enqueued messages without a real outbox. It satisfies
// MailQueue so identity flows can be tested without delivery.
type recordingQueue struct {
	messages []mailoutbox.QueuedMessage
	nudges   int
}

func (q *recordingQueue) Enqueue(_ context.Context, _ *sql.Tx, msg mailoutbox.QueuedMessage) error {
	q.messages = append(q.messages, msg)
	return nil
}
func (q *recordingQueue) Nudge() { q.nudges++ }

func newAccountStore(t *testing.T) (*Store, *sql.DB, *recordingQueue) {
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
	q := &recordingQueue{}
	st.SetMailQueue(q)
	return st, db.SQL, q
}

func signupAndVerify(t *testing.T, st *Store, email, password string) (User, UserSession) {
	t.Helper()
	ctx := context.Background()
	result, err := st.CreateSignup(ctx, email, password)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := st.ConsumeVerification(ctx, result.VerificationToken)
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateUserSession(ctx, verified)
	if err != nil {
		t.Fatal(err)
	}
	return verified, session
}

func TestSignupEnqueuesVerificationMail(t *testing.T) {
	st, db, q := newAccountStore(t)
	ctx := context.Background()
	result, err := st.CreateSignup(ctx, "queue@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.messages) != 1 || q.messages[0].Type != mailoutbox.TypeVerifyEmail {
		t.Fatalf("signup enqueued = %+v", q.messages)
	}
	if q.messages[0].Token != result.VerificationToken {
		t.Fatal("enqueued token does not match the returned token")
	}
	// The queue is a recording fake, so no row is written; the real outbox path
	// is covered in the mailoutbox package tests.
	_ = db
}

func TestChangePasswordKeepsCurrentSessionAndRevokesOthers(t *testing.T) {
	st, _, _ := newAccountStore(t)
	ctx := context.Background()
	u, session := signupAndVerify(t, st, "pw@example.com", "correct horse battery staple")
	other, err := st.CreateUserSession(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ChangePassword(ctx, u.ID, session.Token, "correct horse battery staple", "brand new correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetUserSession(ctx, session.Token); !ok {
		t.Fatal("current session was revoked")
	}
	if _, ok := st.GetUserSession(ctx, other.Token); ok {
		t.Fatal("other session survived the password change")
	}
	if err := st.VerifyPassword(ctx, u.ID, "brand new correct horse battery"); err != nil {
		t.Fatalf("new password does not verify: %v", err)
	}
}

func TestChangePasswordRejectsWrongCurrent(t *testing.T) {
	st, _, _ := newAccountStore(t)
	u, session := signupAndVerify(t, st, "wrong@example.com", "correct horse battery staple")
	if _, err := st.ChangePassword(context.Background(), u.ID, session.Token, "not the password", "another correct horse battery"); err == nil {
		t.Fatal("wrong current password was accepted")
	}
}

func TestRequestEmailChangeEnqueuesConfirmAndWarning(t *testing.T) {
	st, _, q := newAccountStore(t)
	ctx := context.Background()
	u, session := signupAndVerify(t, st, "old@example.com", "correct horse battery staple")
	q.messages = nil
	token, err := st.RequestEmailChange(ctx, u.ID, session.Token, "New@Example.COM")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("no change token returned")
	}
	if len(q.messages) != 2 {
		t.Fatalf("enqueued %d messages, want 2", len(q.messages))
	}
	var confirm, warn *mailoutbox.QueuedMessage
	for i := range q.messages {
		switch q.messages[i].Type {
		case mailoutbox.TypeEmailChangeConfirm:
			confirm = &q.messages[i]
		case mailoutbox.TypeEmailChangeWarning:
			warn = &q.messages[i]
		}
	}
	if confirm == nil || confirm.Recipient != "new@example.com" || confirm.Token == "" {
		t.Fatalf("confirmation message = %+v", confirm)
	}
	if warn == nil || warn.Recipient != "old@example.com" || warn.Token != "" {
		t.Fatalf("warning message = %+v", warn)
	}
}

func TestRequestEmailChangeTakenIsSilent(t *testing.T) {
	st, _, q := newAccountStore(t)
	ctx := context.Background()
	if _, err := st.CreateSignup(ctx, "taken@example.com", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	u, session := signupAndVerify(t, st, "owner@example.com", "correct horse battery staple")
	q.messages = nil
	if _, err := st.RequestEmailChange(ctx, u.ID, session.Token, "taken@example.com"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("taken email error = %v, want ErrEmailTaken", err)
	}
	if len(q.messages) != 0 {
		t.Fatalf("taken email enqueued %d messages, want 0", len(q.messages))
	}
}

func TestConfirmEmailChangeKeepsInitiatingSession(t *testing.T) {
	st, db, _ := newAccountStore(t)
	ctx := context.Background()
	u, session := signupAndVerify(t, st, "before@example.com", "correct horse battery staple")
	other, err := st.CreateUserSession(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.RequestEmailChange(ctx, u.ID, session.Token, "after@example.com")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := st.ConfirmEmailChange(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Email != "after@example.com" {
		t.Fatalf("email after confirm = %q", updated.Email)
	}
	if _, ok := st.GetUserSession(ctx, session.Token); !ok {
		t.Fatal("initiating session was revoked")
	}
	if _, ok := st.GetUserSession(ctx, other.Token); ok {
		t.Fatal("other session survived the email change")
	}
	if _, err := st.ConfirmEmailChange(ctx, token); err == nil {
		t.Fatal("email-change token was reusable")
	}
	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM email_change_tokens`).Scan(&stored); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("email-change token row not cleared after confirm: %v", err)
	}
}

func TestPasswordResetCancelsPendingEmailChange(t *testing.T) {
	st, db, _ := newAccountStore(t)
	ctx := context.Background()
	u, session := signupAndVerify(t, st, "cancel@example.com", "correct horse battery staple")
	if _, err := st.RequestEmailChange(ctx, u.ID, session.Token, "stolen@example.com"); err != nil {
		t.Fatal(err)
	}
	_, resetToken, err := st.IssuePasswordReset(ctx, u.Email)
	if err != nil || resetToken == "" {
		t.Fatalf("reset token = %q, err = %v", resetToken, err)
	}
	if _, err := st.ConsumePasswordReset(ctx, resetToken, "another correct horse battery"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM email_change_tokens WHERE user_id=?`, u.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("email-change tokens after password reset = %d, want 0", count)
	}
}

func TestChangePasswordCancelsPendingEmailChange(t *testing.T) {
	st, db, _ := newAccountStore(t)
	ctx := context.Background()
	u, session := signupAndVerify(t, st, "cancelpw@example.com", "correct horse battery staple")
	if _, err := st.RequestEmailChange(ctx, u.ID, session.Token, "steal@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ChangePassword(ctx, u.ID, session.Token, "correct horse battery staple", "another correct horse battery"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM email_change_tokens WHERE user_id=?`, u.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("email-change tokens after password change = %d, want 0", count)
	}
}

func TestConfirmEmailChangeRaceIsRejected(t *testing.T) {
	st, _, _ := newAccountStore(t)
	ctx := context.Background()
	u, session := signupAndVerify(t, st, "race@example.com", "correct horse battery staple")
	token, err := st.RequestEmailChange(ctx, u.ID, session.Token, "want@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Another account claims the address between request and confirm.
	if _, err := st.CreateSignup(ctx, "want@example.com", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConfirmEmailChange(ctx, token); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("confirm race error = %v, want ErrEmailTaken", err)
	}
}

func TestAccountProfileReportsIdentity(t *testing.T) {
	st, _, _ := newAccountStore(t)
	ctx := context.Background()
	u, _ := signupAndVerify(t, st, "profile@example.com", "correct horse battery staple")
	profile, err := st.AccountProfile(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Email != "profile@example.com" || profile.AccountID != u.AccountID || !profile.Verified || profile.Plan != "free" {
		t.Fatalf("profile = %+v", profile)
	}
}

func TestRevokeAllUserSessionsRevokesEverySession(t *testing.T) {
	st, _, _ := newAccountStore(t)
	ctx := context.Background()
	u, session := signupAndVerify(t, st, "revokeall@example.com", "correct horse battery staple")
	other, err := st.CreateUserSession(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeAllUserSessions(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetUserSession(ctx, session.Token); ok {
		t.Fatal("session survived revoke-all")
	}
	if _, ok := st.GetUserSession(ctx, other.Token); ok {
		t.Fatal("other session survived revoke-all")
	}
}

func TestPruneExpiredTokens(t *testing.T) {
	st, db, _ := newAccountStore(t)
	ctx := context.Background()
	u, _ := signupAndVerify(t, st, "prune@example.com", "correct horse battery staple")
	if _, err := st.RequestEmailChange(ctx, u.ID, mustNewSession(t, st, u).Token, "prune-new@example.com"); err != nil {
		t.Fatal(err)
	}
	// Force the token to the past, then prune.
	if _, err := db.Exec(`UPDATE email_change_tokens SET expires_at=?`, formatTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := st.PruneExpiredTokens(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM email_change_tokens`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expired email-change tokens after prune = %d, want 0", count)
	}
}

func mustNewSession(t *testing.T, st *Store, u User) UserSession {
	t.Helper()
	s, err := st.CreateUserSession(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
