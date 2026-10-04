package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
)

// activeUser creates a verified active hosted user for passkey tests.
func activeUser(t *testing.T, st *Store) User {
	t.Helper()
	ctx := context.Background()
	result, err := st.CreateSignup(ctx, "user@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	verified, err := st.ConsumeVerification(ctx, result.VerificationToken)
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

func insertPasskey(t *testing.T, st *Store, userID string, marker byte, name string) string {
	t.Helper()
	ctx := context.Background()
	cred := webauthn.Credential{ID: []byte{marker, marker, marker, marker}, PublicKey: []byte{marker, marker, marker, marker, marker, marker}}
	if err := st.addPasskey(ctx, userID, cred, name); err != nil {
		t.Fatal(err)
	}
	keys, err := st.ListPasskeys(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Name == name {
			return k.ID
		}
	}
	t.Fatalf("passkey %q not found after insert", name)
	return ""
}

func TestPasskeyLifecycleAndLastMethodGuard(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)

	first := insertPasskey(t, st, u.ID, 1, "Laptop")
	insertPasskey(t, st, u.ID, 2, "Phone")
	if n, err := st.PasskeyCount(ctx, u.ID); err != nil || n != 2 {
		t.Fatalf("count = %d, %v", n, err)
	}
	// Rename is scoped to the owner.
	if err := st.RenamePasskey(ctx, u.ID, first, "Work laptop"); err != nil {
		t.Fatal(err)
	}
	if err := st.RenamePasskey(ctx, "someone-else", first, "hijack"); !errors.Is(err, ErrPasskeyNotFound) {
		t.Fatalf("cross-user rename err = %v, want ErrPasskeyNotFound", err)
	}
	// Delete with two methods succeeds.
	if err := st.DeletePasskey(ctx, u.ID, first); err != nil {
		t.Fatal(err)
	}
	// Disable password, then the single remaining passkey cannot be deleted.
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}
	remaining, _ := st.ListPasskeys(ctx, u.ID)
	if err := st.DeletePasskey(ctx, u.ID, remaining[0].ID); !errors.Is(err, ErrLastAuthMethod) {
		t.Fatalf("delete last method err = %v, want ErrLastAuthMethod", err)
	}
}

func TestSetPasswordSignInRefusesWithoutPasskey(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, false); !errors.Is(err, ErrLastAuthMethod) {
		t.Fatalf("disable without passkey err = %v, want ErrLastAuthMethod", err)
	}
	insertPasskey(t, st, u.ID, 1, "Laptop")
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err := st.UserByEmail(ctx, u.Email)
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordEnabled {
		t.Fatal("password still enabled after disable")
	}
	// A passkey-only user cannot authenticate with the password.
	if _, err := st.AuthenticatePassword(ctx, u.Email, "correct horse battery staple"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("password login for passkey-only user err = %v, want ErrNotFound", err)
	}
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
}

func TestPasskeysDisabledUntilConfigured(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)
	if st.PasskeysEnabled() {
		t.Fatal("passkeys enabled before ConfigureWebAuthn")
	}
	if _, err := st.BeginPasskeyLogin(); !errors.Is(err, ErrWebAuthnUnavailable) {
		t.Fatalf("BeginPasskeyLogin err = %v, want ErrWebAuthnUnavailable", err)
	}
	if err := st.ConfigureWebAuthn(WebAuthnConfig{RPDisplayName: "Tiller", RPID: "example.com", Origins: []string{"https://example.com"}}); err != nil {
		t.Fatal(err)
	}
	if !st.PasskeysEnabled() {
		t.Fatal("passkeys not enabled after ConfigureWebAuthn")
	}
	opts, err := st.BeginPasskeyLogin()
	if err != nil {
		t.Fatal(err)
	}
	if opts.ChallengeToken == "" || opts.Options == nil {
		t.Fatalf("discoverable login options incomplete: %+v", opts)
	}
	// Registration begins for a user.
	reg, err := st.BeginPasskeyRegistration(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if reg.ChallengeToken == "" || reg.Options == nil {
		t.Fatalf("registration options incomplete: %+v", reg)
	}
}

func TestConfigureWebAuthnRequiresOrigin(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.ConfigureWebAuthn(WebAuthnConfig{RPID: "example.com"}); err == nil {
		t.Fatal("expected error when no origin supplied")
	}
}
