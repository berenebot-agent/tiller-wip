package identity

import (
	"context"
	"errors"
	"testing"
)

// The auth-generation race: a login reads a User snapshot, then a credential
// mutation lands before CreateUserSession runs. CreateUserSession re-checks the
// generation and must refuse, otherwise a session is minted from credential
// state that no longer exists. These tests reproduce the interleaving
// deterministically by holding the stale snapshot across the mutation, which is
// exactly what the real concurrent request does.

// TestLinkRejectsPasswordLoginSnapshotTakenBeforeLink covers password login
// racing with Google linking. Linking disables password auth, so a password
// authentication that observed the pre-link state must not be able to complete.
func TestLinkRejectsPasswordLoginSnapshotTakenBeforeLink(t *testing.T) {
	st, _, _ := newAccountStore(t)
	ctx := context.Background()
	u, _ := signupAndVerify(t, st, "link-race@example.com", "correct horse battery staple")

	// This is what AuthenticatePassword returns: a snapshot with the current
	// (pre-link) generation.
	stale := u

	if err := st.LinkGoogleIdentity(ctx, u.ID, "google-subject-link-race", u.Email); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := st.CreateUserSession(ctx, stale); !errors.Is(err, ErrStaleAuthentication) {
		t.Fatalf("session from pre-link snapshot = %v, want ErrStaleAuthentication", err)
	}
	// A fresh read succeeds: the account is still usable after linking.
	fresh, err := st.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUserSession(ctx, fresh); err != nil {
		t.Fatalf("session from post-link snapshot: %v", err)
	}
}

// TestUnlinkRejectsGoogleLoginSnapshotTakenBeforeUnlink covers a Google sign-in
// racing with unlink. Unlink deletes the Google identity, so a session created
// from a snapshot taken while the identity was still attached must be refused:
// this is the case where a revoked sign-in method would otherwise still mint a
// session after the user revoked it.
func TestUnlinkRejectsGoogleLoginSnapshotTakenBeforeUnlink(t *testing.T) {
	st, _, _ := newAccountStore(t)
	ctx := context.Background()
	u, _ := signupAndVerify(t, st, "unlink-race@example.com", "correct horse battery staple")
	if err := st.LinkGoogleIdentity(ctx, u.ID, "google-subject-unlink-race", u.Email); err != nil {
		t.Fatalf("link: %v", err)
	}

	// This is what GoogleUserBySubject returns before the unlink. Keep the
	// snapshot's generation current — the race is that unlink did not (before
	// the fix) change it, so only an up-to-date read models the real handler,
	// which reads immediately before the unlink.
	stale, err := st.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.UnlinkGoogleIdentity(ctx, u.ID); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := st.CreateUserSession(ctx, stale); !errors.Is(err, ErrStaleAuthentication) {
		t.Fatalf("session from pre-unlink snapshot = %v, want ErrStaleAuthentication", err)
	}
}

// TestGoogleLinkBumpsGeneration proves the generation actually changes on link
// and unlink, which is the mechanism the two race tests above depend on.
func TestGoogleLinkBumpsGeneration(t *testing.T) {
	st, _, _ := newAccountStore(t)
	ctx := context.Background()
	u, _ := signupAndVerify(t, st, "generation@example.com", "correct horse battery staple")

	before, err := st.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkGoogleIdentity(ctx, u.ID, "google-subject-generation", u.Email); err != nil {
		t.Fatalf("link: %v", err)
	}
	afterLink, err := st.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterLink.AuthGeneration != before.AuthGeneration+1 {
		t.Fatalf("link generation = %d, want %d", afterLink.AuthGeneration, before.AuthGeneration+1)
	}

	if err := st.UnlinkGoogleIdentity(ctx, u.ID); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	afterUnlink, err := st.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterUnlink.AuthGeneration != afterLink.AuthGeneration+1 {
		t.Fatalf("unlink generation = %d, want %d", afterUnlink.AuthGeneration, afterLink.AuthGeneration+1)
	}
}

// TestRelinkSameSubjectBumpsGeneration documents the deliberate side effect:
// re-linking an identity that is already attached still disables password auth,
// so it still counts as a credential change and revokes existing sessions.
func TestRelinkSameSubjectBumpsGeneration(t *testing.T) {
	st, _, _ := newAccountStore(t)
	ctx := context.Background()
	u, first := signupAndVerify(t, st, "relink@example.com", "correct horse battery staple")

	if err := st.LinkGoogleIdentity(ctx, u.ID, "google-subject-relink", u.Email); err != nil {
		t.Fatalf("first link: %v", err)
	}
	afterFirst, err := st.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkGoogleIdentity(ctx, u.ID, "google-subject-relink", u.Email); err != nil {
		t.Fatalf("re-link: %v", err)
	}
	afterSecond, err := st.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterSecond.AuthGeneration != afterFirst.AuthGeneration+1 {
		t.Fatalf("re-link generation = %d, want %d", afterSecond.AuthGeneration, afterFirst.AuthGeneration+1)
	}
	if _, ok := st.GetUserSession(ctx, first.Token); ok {
		t.Fatal("session survived the re-link revocation")
	}
}
