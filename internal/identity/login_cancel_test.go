package identity

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// abandonedHasher reports that the KDF wait ended before a slot was free, as the
// production Argon2 hasher does when the request context is done.
type abandonedHasher struct {
	fastsecret.Hasher
	abandon bool
}

func (h abandonedHasher) VerifyContext(_ context.Context, secret, encoded string) (bool, bool) {
	if h.abandon {
		return false, false
	}
	return h.Hasher.Verify(secret, encoded), true
}

// TestAuthenticatePasswordAbandonedWaitIsNotAWrongPassword proves public login
// gives up its KDF queue place for a caller that went away, for a known account
// and for the dummy verification an unknown email costs, and reports it as
// cancellation rather than a wrong password.
func TestAuthenticatePasswordAbandonedWaitIsNotAWrongPassword(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := New(db.SQL, abandonedHasher{}, fastsecret.Hasher{}, fastsecret.Hasher{}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := st.CreateSignup(ctx, "known@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumeVerification(ctx, res.VerificationToken, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuthenticatePassword(ctx, "known@example.com", "correct horse battery staple"); err != nil {
		t.Fatalf("live wait should authenticate: %v", err)
	}
	st.passwordHasher = abandonedHasher{abandon: true}
	if _, err := st.AuthenticatePassword(ctx, "known@example.com", "correct horse battery staple"); !errors.Is(err, ErrReauthenticationCancelled) {
		t.Fatalf("known account, abandoned wait: err = %v, want ErrReauthenticationCancelled", err)
	}
	if _, err := st.AuthenticatePassword(ctx, "nobody@example.com", "whatever password"); !errors.Is(err, ErrReauthenticationCancelled) {
		t.Fatalf("unknown email, abandoned wait: err = %v, want ErrReauthenticationCancelled", err)
	}
}
