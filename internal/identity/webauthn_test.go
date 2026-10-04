package identity

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
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
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, "", false); err != nil {
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
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, "", false); !errors.Is(err, ErrLastAuthMethod) {
		t.Fatalf("disable without passkey err = %v, want ErrLastAuthMethod", err)
	}
	insertPasskey(t, st, u.ID, 1, "Laptop")
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, "", false); err != nil {
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
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, "", true); err != nil {
		t.Fatal(err)
	}
}

// TestSetPasswordSignInKeepsInitiatingSession verifies the session that made
// the change survives while every other session is revoked.
func TestSetPasswordSignInKeepsInitiatingSession(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)
	insertPasskey(t, st, u.ID, 1, "Laptop")
	fresh, err := st.UserByEmail(ctx, u.Email)
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.CreateUserSession(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateUserSession(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, first.Token, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetUserSession(ctx, first.Token); !ok {
		t.Fatal("initiating session was revoked by its own password-signin change")
	}
	if _, ok := st.GetUserSession(ctx, second.Token); ok {
		t.Fatal("other session survived the password-signin change")
	}
}

// TestPasskeyOnlyResetRecoversAccount covers the loss-of-last-passkey recovery
// path: a passkey-only account can still request and consume a password reset,
// which restores password sign-in.
func TestPasskeyOnlyResetRecoversAccount(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)
	insertPasskey(t, st, u.ID, 1, "Laptop")
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, "", false); err != nil {
		t.Fatal(err)
	}
	_, raw, err := st.IssuePasswordReset(ctx, u.Email)
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("passkey-only account was not issued a recovery token")
	}
	if _, err := st.ConsumePasswordReset(ctx, raw, "a brand new recovery password"); err != nil {
		t.Fatal(err)
	}
	got, err := st.UserByEmail(ctx, u.Email)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PasswordEnabled {
		t.Fatal("password sign-in was not restored by passkey-only recovery")
	}
	if _, err := st.AuthenticatePassword(ctx, u.Email, "a brand new recovery password"); err != nil {
		t.Fatalf("recovered password does not authenticate: %v", err)
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

// TestChallengeStoreEvictsOldestWhenFull proves a flood cannot permanently
// deny ceremonies: when the store is at capacity the newest begin succeeds by
// evicting the entry nearest expiry.
func TestChallengeStoreEvictsOldestWhenFull(t *testing.T) {
	st := newChallengeStore()
	for i := 0; i < maxPendingChallenges; i++ {
		if _, ok := st.put(challengeLogin, "", webauthn.SessionData{Challenge: "x"}); !ok {
			t.Fatalf("put %d refused before capacity", i)
		}
	}
	token, ok := st.put(challengeLogin, "", webauthn.SessionData{Challenge: "y"})
	if !ok {
		t.Fatal("put refused at capacity instead of evicting the oldest entry")
	}
	if _, _, ok := st.take(token, challengeLogin); !ok {
		t.Fatal("newly stored ceremony is not retrievable")
	}
	if len(st.entries) != maxPendingChallenges-1 {
		t.Fatalf("store size after eviction = %d, want %d", len(st.entries), maxPendingChallenges-1)
	}
}

// TestCredentialsForUserRestoresBackupFlags is the regression guard for the
// synced-passkey login failure: the library rejects an assertion when the
// stored BackupEligible flag differs from the authenticator's, so both backup
// flags and transports must round-trip.
func TestCredentialsForUserRestoresBackupFlags(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)
	cred := webauthn.Credential{
		ID:        []byte("synced-credential-id"),
		PublicKey: []byte("public-key-bytes"),
		Flags:     webauthn.CredentialFlags{BackupEligible: true, BackupState: true, UserVerified: true},
		Transport: []protocol.AuthenticatorTransport{protocol.Hybrid, protocol.Internal},
	}
	if err := st.addPasskey(ctx, u.ID, cred, "iCloud"); err != nil {
		t.Fatal(err)
	}
	got, err := st.credentialsForUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("credentials = %d, want 1", len(got))
	}
	if !got[0].Flags.BackupEligible || !got[0].Flags.BackupState {
		t.Fatalf("backup flags lost: %+v", got[0].Flags)
	}
	if len(got[0].Transport) != 2 || got[0].Transport[0] != protocol.Hybrid || got[0].Transport[1] != protocol.Internal {
		t.Fatalf("transports lost: %+v", got[0].Transport)
	}
}

// TestRecordPasskeyUseCounterSemantics exercises the atomic counter update:
// advancing counters persist, a non-advancing counter reports a clone warning
// and does not regress the stored value, and a zero counter neither advances
// nor warns.
func TestRecordPasskeyUseCounterSemantics(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)
	cred := webauthn.Credential{ID: []byte("counter-cred"), PublicKey: []byte("counter-key")}
	if err := st.addPasskey(ctx, u.ID, cred, "Key"); err != nil {
		t.Fatal(err)
	}
	clone, err := st.recordPasskeyUse(ctx, cred.ID, 5, false)
	if err != nil || clone {
		t.Fatalf("first use = clone %v err %v", clone, err)
	}
	clone, err = st.recordPasskeyUse(ctx, cred.ID, 6, true)
	if err != nil || clone {
		t.Fatalf("advance = clone %v err %v", clone, err)
	}
	clone, err = st.recordPasskeyUse(ctx, cred.ID, 6, true)
	if err != nil || !clone {
		t.Fatalf("non-advance = clone %v err %v, want clone warning", clone, err)
	}
	var stored, backupState int
	if err := st.db.QueryRowContext(ctx, `SELECT sign_count,backup_state FROM webauthn_credentials WHERE credential_id=?`, cred.ID).Scan(&stored, &backupState); err != nil {
		t.Fatal(err)
	}
	if stored != 6 {
		t.Fatalf("stored counter regressed to %d, want 6", stored)
	}
	if backupState != 1 {
		t.Fatalf("backup state not updated, got %d", backupState)
	}
}

func TestCredentialByCredentialIDNotFound(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := st.credentialByCredentialID(context.Background(), []byte("missing")); !errors.Is(err, ErrPasskeyNotFound) {
		t.Fatalf("err = %v, want ErrPasskeyNotFound", err)
	}
}

// ---- End-to-end ceremony fixtures ----

// testAuthenticator is a minimal software authenticator used to build real
// registration and assertion responses against the store's ceremonies. It
// signs with a generated P-256 key and serialises per the WebAuthn spec.
type testAuthenticator struct {
	key          *ecdsa.PrivateKey
	credentialID []byte
	aaguid       []byte
	be, bs       bool
	counter      uint32
}

func newTestAuthenticator(t *testing.T, credentialID string) *testAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testAuthenticator{key: key, credentialID: []byte(credentialID), aaguid: make([]byte, 16)}
}

// cosePublicKey encodes the P-256 public key in COSE_Key canonical CBOR.
func (a *testAuthenticator) cosePublicKey(t *testing.T) []byte {
	t.Helper()
	x := a.key.PublicKey.X.FillBytes(make([]byte, 32))
	y := a.key.PublicKey.Y.FillBytes(make([]byte, 32))
	encoded, err := cbor.Marshal(map[int]any{
		1:  2,  // kty: EC2
		3:  -7, // alg: ES256
		-1: 1,  // crv: P-256
		-2: x,
		-3: y,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func (a *testAuthenticator) flags(attested bool) protocol.AuthenticatorFlags {
	flags := protocol.FlagUserPresent | protocol.FlagUserVerified
	if attested {
		flags |= protocol.FlagAttestedCredentialData
	}
	if a.be {
		flags |= protocol.FlagBackupEligible
	}
	if a.bs {
		flags |= protocol.FlagBackupState
	}
	return flags
}

// authData builds the authenticator data blob. With attested=true it appends
// the attested credential data (AAGUID, id length, id, COSE key).
func (a *testAuthenticator) authData(t *testing.T, rpID string, attested bool) []byte {
	t.Helper()
	hash := sha256.Sum256([]byte(rpID))
	var buf bytes.Buffer
	buf.Write(hash[:])
	buf.WriteByte(byte(a.flags(attested)))
	counter := make([]byte, 4)
	binary.BigEndian.PutUint32(counter, a.counter)
	buf.Write(counter)
	if attested {
		buf.Write(a.aaguid)
		idLen := make([]byte, 2)
		binary.BigEndian.PutUint16(idLen, uint16(len(a.credentialID)))
		buf.Write(idLen)
		buf.Write(a.credentialID)
		buf.Write(a.cosePublicKey(t))
	}
	return buf.Bytes()
}

// clientData builds the collected client data JSON for a ceremony.
func clientData(ceremony protocol.CeremonyType, challenge, origin string) []byte {
	data, _ := json.Marshal(map[string]any{
		"type":        string(ceremony),
		"challenge":   challenge,
		"origin":      origin,
		"crossOrigin": false,
	})
	return data
}

// registrationResponse builds the JSON body the finish endpoint parses.
func (a *testAuthenticator) registrationResponse(t *testing.T, rpID, challenge, origin string) *http.Request {
	t.Helper()
	authData := a.authData(t, rpID, true)
	attObj, err := cbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id":    base64.RawURLEncoding.EncodeToString(a.credentialID),
		"rawId": base64.RawURLEncoding.EncodeToString(a.credentialID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData(protocol.CreateCeremony, challenge, origin)),
			"attestationObject": base64.RawURLEncoding.EncodeToString(attObj),
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/finish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// assertionResponse signs a real assertion over authData || SHA256(clientData).
// userHandle is what the authenticator returns for a discoverable credential;
// it must match the user's WebAuthnID for the assertion to validate.
func (a *testAuthenticator) assertionResponse(t *testing.T, rpID, challenge, origin, userHandle string) *http.Request {
	t.Helper()
	authData := a.authData(t, rpID, false)
	cd := clientData(protocol.AssertCeremony, challenge, origin)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	response := map[string]any{
		"clientDataJSON":    base64.RawURLEncoding.EncodeToString(cd),
		"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
		"signature":         base64.RawURLEncoding.EncodeToString(sig),
	}
	if userHandle != "" {
		response["userHandle"] = base64.RawURLEncoding.EncodeToString([]byte(userHandle))
	}
	body, _ := json.Marshal(map[string]any{
		"id":       base64.RawURLEncoding.EncodeToString(a.credentialID),
		"rawId":    base64.RawURLEncoding.EncodeToString(a.credentialID),
		"type":     "public-key",
		"response": response,
	})
	req := httptest.NewRequest(http.MethodPost, "/finish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func configureTestWebAuthn(t *testing.T, st *Store) {
	t.Helper()
	if err := st.ConfigureWebAuthn(WebAuthnConfig{RPDisplayName: "Tiller", RPID: "example.com", Origins: []string{"https://example.com"}}); err != nil {
		t.Fatal(err)
	}
}

// TestFinishPasskeyRegistrationAndLoginE2E drives a full registration and a
// discoverable login with a real software authenticator, proving the stored
// key and the ceremony plumbing work end to end. It also proves the login
// succeeds for a synced passkey (BackupEligible=true), which the old
// credentialsForUser regression made impossible.
func TestFinishPasskeyRegistrationAndLoginE2E(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	configureTestWebAuthn(t, st)
	u := activeUser(t, st)

	auth := newTestAuthenticator(t, "e2e-credential-id")
	auth.be, auth.bs = true, true

	reg, err := st.BeginPasskeyRegistration(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	regChallenge := reg.Options.Response.Challenge.String()
	if err := st.FinishPasskeyRegistration(ctx, u, reg.ChallengeToken, "iCloud keychain", auth.registrationResponse(t, "example.com", regChallenge, "https://example.com")); err != nil {
		t.Fatalf("registration failed: %v", err)
	}
	if n, err := st.PasskeyCount(ctx, u.ID); err != nil || n != 1 {
		t.Fatalf("count after registration = %d, %v", n, err)
	}

	login, err := st.BeginPasskeyLogin()
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := st.FinishPasskeyLogin(ctx, login.ChallengeToken, auth.assertionResponse(t, "example.com", login.Options.Response.Challenge.String(), "https://example.com", u.ID))
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if assertion.User.ID != u.ID {
		t.Fatalf("login resolved user %q, want %q", assertion.User.ID, u.ID)
	}
	if assertion.CloneWarning || assertion.RecordErr != nil {
		t.Fatalf("unexpected diagnostics: clone=%v err=%v", assertion.CloneWarning, assertion.RecordErr)
	}
	// A second login with an advanced counter must not be a clone warning.
	auth.counter = 1
	login2, err := st.BeginPasskeyLogin()
	if err != nil {
		t.Fatal(err)
	}
	assertion2, err := st.FinishPasskeyLogin(ctx, login2.ChallengeToken, auth.assertionResponse(t, "example.com", login2.Options.Response.Challenge.String(), "https://example.com", u.ID))
	if err != nil {
		t.Fatalf("second login failed: %v", err)
	}
	if assertion2.CloneWarning {
		t.Fatal("advancing counter reported as clone warning")
	}
}

// TestFinishPasskeyLoginRejectsWrongUserHandle proves the user-handle binding:
// an assertion whose handle does not match the credential owner is rejected.
func TestFinishPasskeyLoginRejectsWrongUserHandle(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	configureTestWebAuthn(t, st)
	u := activeUser(t, st)
	auth := newTestAuthenticator(t, "handle-credential-id")
	auth.be = true

	reg, err := st.BeginPasskeyRegistration(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishPasskeyRegistration(ctx, u, reg.ChallengeToken, "Key", auth.registrationResponse(t, "example.com", reg.Options.Response.Challenge.String(), "https://example.com")); err != nil {
		t.Fatal(err)
	}
	login, err := st.BeginPasskeyLogin()
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.FinishPasskeyLogin(ctx, login.ChallengeToken, auth.assertionResponse(t, "example.com", login.Options.Response.Challenge.String(), "https://example.com", "not-the-right-handle"))
	if err == nil {
		t.Fatal("assertion with a mismatched user handle was accepted")
	}
}

// TestFinishPasskeyRegistrationRejectsDuplicateCredential proves a duplicate
// credential id is a conflict, never a silent overwrite.
func TestFinishPasskeyRegistrationRejectsDuplicateCredential(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	configureTestWebAuthn(t, st)
	u := activeUser(t, st)
	auth := newTestAuthenticator(t, "dup-credential-id")

	reg, err := st.BeginPasskeyRegistration(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	challenge := reg.Options.Response.Challenge.String()
	if err := st.FinishPasskeyRegistration(ctx, u, reg.ChallengeToken, "First", auth.registrationResponse(t, "example.com", challenge, "https://example.com")); err != nil {
		t.Fatal(err)
	}
	reg2, err := st.BeginPasskeyRegistration(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	challenge2 := reg2.Options.Response.Challenge.String()
	err = st.FinishPasskeyRegistration(ctx, u, reg2.ChallengeToken, "Second", auth.registrationResponse(t, "example.com", challenge2, "https://example.com"))
	if !errors.Is(err, ErrAlreadyUsed) {
		t.Fatalf("duplicate registration err = %v, want ErrAlreadyUsed", err)
	}
}

// TestReauthCeremonyE2E drives the re-auth assertion path used by sensitive
// account operations.
func TestReauthCeremonyE2E(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	configureTestWebAuthn(t, st)
	u := activeUser(t, st)
	auth := newTestAuthenticator(t, "reauth-credential-id")

	reg, err := st.BeginPasskeyRegistration(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	regChallenge := reg.Options.Response.Challenge.String()
	if err := st.FinishPasskeyRegistration(ctx, u, reg.ChallengeToken, "Laptop", auth.registrationResponse(t, "example.com", regChallenge, "https://example.com")); err != nil {
		t.Fatal(err)
	}
	begin, err := st.BeginPasskeyReauth(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	challenge := begin.Options.Response.Challenge.String()
	if err := st.FinishPasskeyReauth(ctx, u, begin.ChallengeToken, auth.assertionResponse(t, "example.com", challenge, "https://example.com", u.ID)); err != nil {
		t.Fatalf("re-auth failed: %v", err)
	}
	// A different user cannot spend the ceremony token even if it leaks.
	other := User{ID: "someone-else"}
	begin2, err := st.BeginPasskeyReauth(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishPasskeyReauth(ctx, other, begin2.ChallengeToken, auth.assertionResponse(t, "example.com", begin2.Options.Response.Challenge.String(), "https://example.com", u.ID)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("cross-user reauth err = %v, want ErrInvalidToken", err)
	}
}

// TestBeginPasskeyReauthRequiresCredential proves re-auth is refused when the
// user has no registered passkey, rather than returning an unusable ceremony.
func TestBeginPasskeyReauthRequiresCredential(t *testing.T) {
	st, _ := newTestStore(t)
	configureTestWebAuthn(t, st)
	u := activeUser(t, st)
	if _, err := st.BeginPasskeyReauth(context.Background(), u); !errors.Is(err, ErrPasskeyNotFound) {
		t.Fatalf("BeginPasskeyReauth err = %v, want ErrPasskeyNotFound", err)
	}
}

func TestAddPasskeyRejectsOversizedName(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)
	cred := webauthn.Credential{ID: []byte("name-cred"), PublicKey: []byte("name-key")}
	if err := st.addPasskey(ctx, u.ID, cred, strings.Repeat("n", 81)); err == nil {
		t.Fatal("expected 81-character passkey name to be rejected")
	}
}

// TestPasskeyNameRenameValidation proves rename enforces non-empty and bounded
// names.
func TestPasskeyNameRenameValidation(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)
	row := insertPasskey(t, st, u.ID, 9, "Key")
	if err := st.RenamePasskey(ctx, u.ID, row, "   "); err == nil {
		t.Fatal("expected blank rename to be rejected")
	}
	if err := st.RenamePasskey(ctx, u.ID, row, strings.Repeat("n", 81)); err == nil {
		t.Fatal("expected oversized rename to be rejected")
	}
}

// TestDeletePasskeyGoogleLinkedKeepsAccess proves a Google-linked user with one
// passkey can remove it: Google remains a sign-in method.
func TestDeletePasskeyGoogleLinkedKeepsAccess(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)
	row := insertPasskey(t, st, u.ID, 1, "Laptop")
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkGoogleIdentity(ctx, u.ID, "google-subject-1", u.Email); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePasskey(ctx, u.ID, row); err != nil {
		t.Fatalf("Google-linked user could not delete their only passkey: %v", err)
	}
}

// TestEnablePasswordRefusedWhileGoogleLinked proves the Google invariant:
// password auth cannot be switched back on for a Google-linked account.
func TestEnablePasswordRefusedWhileGoogleLinked(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)
	if err := st.LinkGoogleIdentity(ctx, u.ID, "google-subject-2", u.Email); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, "", true); !errors.Is(err, ErrPasswordDisabled) {
		t.Fatalf("enable while Google-linked err = %v, want ErrPasswordDisabled", err)
	}
}

// TestConcurrentDeletePasskeysCannotLockOut exercises the last-method guard
// under concurrent deletes of the final two credentials.
func TestConcurrentDeletePasskeysCannotLockOut(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	u := activeUser(t, st)
	first := insertPasskey(t, st, u.ID, 1, "One")
	second := insertPasskey(t, st, u.ID, 2, "Two")
	if err := st.SetPasswordSignInEnabled(ctx, u.ID, "", false); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { results <- st.DeletePasskey(ctx, u.ID, first) }()
	go func() { results <- st.DeletePasskey(ctx, u.ID, second) }()
	var errs []error
	for i := 0; i < 2; i++ {
		errs = append(errs, <-results)
	}
	remaining, err := st.PasskeyCount(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	// At least one delete must be refused or serialized away: the guard exists
	// so the account always keeps a method. Either one delete succeeded and the
	// other was refused, or both serialized with one refusing.
	if remaining < 1 {
		t.Fatalf("both passkeys deleted (remaining=%d), account locked out; errs=%v", remaining, errs)
	}
}
