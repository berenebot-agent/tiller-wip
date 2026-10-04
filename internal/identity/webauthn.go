package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/tiller-router/tiller-router/internal/id"
)

// WebAuthnConfig configures the relying party for passkeys. RPID is the
// registrable domain (no scheme/port); Origins are the exact origins permitted
// to complete a ceremony (normally the single public HTTPS origin).
type WebAuthnConfig struct {
	RPDisplayName string
	RPID          string
	Origins       []string
}

// Passkey errors.
var (
	ErrWebAuthnUnavailable = errors.New("identity: passkeys are not configured")
	ErrLastAuthMethod      = errors.New("identity: cannot remove the only sign-in method")
	ErrPasskeyNotFound     = errors.New("identity: passkey not found")
)

// challengeTTL bounds how long a begun ceremony may be completed.
const challengeTTL = 10 * time.Minute

// maxPendingChallenges caps the in-memory challenge store against abuse.
const maxPendingChallenges = 4096

type challengeKind int

const (
	challengeRegister challengeKind = iota
	challengeLogin
	challengeReauth
)

type pendingChallenge struct {
	kind    challengeKind
	userID  string
	session webauthn.SessionData
	expires time.Time
}

// challengeStore holds one-use ceremony state in memory. A restart expires every
// ceremony, which is safer than persisting browser authentication state. Keys
// are a hash of the raw token, matching hostedauth.FlowStore's approach.
type challengeStore struct {
	mu      sync.Mutex
	entries map[[32]byte]pendingChallenge
}

func newChallengeStore() *challengeStore {
	return &challengeStore{entries: make(map[[32]byte]pendingChallenge)}
}

func (s *challengeStore) put(kind challengeKind, userID string, session webauthn.SessionData) (string, bool) {
	raw, err := randomURL(32)
	if err != nil {
		return "", false
	}
	key := sha256.Sum256([]byte(raw))
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var oldestKey [32]byte
	var oldest time.Time
	foundOldest := false
	for k, v := range s.entries {
		if !now.Before(v.expires) {
			delete(s.entries, k)
			continue
		}
		if !foundOldest || v.expires.Before(oldest) {
			oldestKey, oldest, foundOldest = k, v.expires, true
		}
	}
	if len(s.entries) >= maxPendingChallenges {
		// Evict the entry closest to expiry instead of refusing the new
		// ceremony. Refusing here would let an attacker who can start
		// (unauthenticated) login ceremonies fill the store and permanently
		// deny every legitimate ceremony; eviction bounds the store without
		// handing the attacker a veto. The evicted entry can only be an
		// unfinished begin whose owner will retry.
		if foundOldest {
			delete(s.entries, oldestKey)
		} else {
			return "", false
		}
	}
	s.entries[key] = pendingChallenge{kind: kind, userID: userID, session: session, expires: now.Add(challengeTTL)}
	return raw, true
}

func (s *challengeStore) take(token string, kind challengeKind) (string, webauthn.SessionData, bool) {
	if token == "" {
		return "", webauthn.SessionData{}, false
	}
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.entries[key]
	delete(s.entries, key)
	if !found || entry.kind != kind || !time.Now().Before(entry.expires) {
		return "", webauthn.SessionData{}, false
	}
	return entry.userID, entry.session, true
}

// webAuthnUser adapts a User and its stored passkeys to the library's User
// interface. The WebAuthnID is the user's row id bytes (stable and unique).
type webAuthnUser struct {
	u     User
	creds []webauthn.Credential
}

func (w *webAuthnUser) WebAuthnID() []byte                         { return []byte(w.u.ID) }
func (w *webAuthnUser) WebAuthnName() string                       { return w.u.Email }
func (w *webAuthnUser) WebAuthnDisplayName() string                { return w.u.Email }
func (w *webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

// ConfigureWebAuthn installs the relying-party configuration. Until it is
// called the passkey endpoints report themselves unavailable, so local mode and
// tests without a public origin simply have no passkeys.
func (s *Store) ConfigureWebAuthn(cfg WebAuthnConfig) error {
	if cfg.RPID == "" || len(cfg.Origins) == 0 {
		return errors.New("identity: webauthn requires an RPID and at least one origin")
	}
	if cfg.RPDisplayName == "" {
		cfg.RPDisplayName = "Tiller"
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPDisplayName: cfg.RPDisplayName,
		RPID:          cfg.RPID,
		RPOrigins:     cfg.Origins,
	})
	if err != nil {
		return fmt.Errorf("identity: webauthn: %w", err)
	}
	s.mu.Lock()
	s.webauthn = wa
	s.challenges = newChallengeStore()
	s.mu.Unlock()
	return nil
}

// passkeysEnabledLocked reports whether WebAuthn has been configured.
func (s *Store) passkeysEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.webauthn != nil
}

// PasskeysEnabled reports whether this deployment supports passkeys, i.e. a
// public origin was configured. It gates the login and account UI.
func (s *Store) PasskeysEnabled() bool { return s.passkeysEnabled() }

// PasskeyInfo is the non-secret account view of a registered passkey.
type PasskeyInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

// storedCredential is a passkey row plus the fields needed for a ceremony.
type storedCredential struct {
	rowID           string
	userID          string
	credentialID    []byte
	publicKey       []byte
	signCount       uint32
	transports      []string
	name            string
	backupEligible  bool
	backupState     bool
	attestationType string
	aaguid          string
	createdAt       string
	lastUsedAt      sql.NullString
}

// ListPasskeys returns a user's registered passkeys for the account page.
func (s *Store) ListPasskeys(ctx context.Context, userID string) ([]PasskeyInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,created_at,last_used_at FROM webauthn_credentials WHERE user_id=? ORDER BY created_at DESC, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PasskeyInfo{}
	for rows.Next() {
		var p PasskeyInfo
		var last sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &p.CreatedAt, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			p.LastUsedAt = last.String
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PasskeyCount returns how many passkeys a user has registered.
func (s *Store) PasskeyCount(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM webauthn_credentials WHERE user_id=?`, userID).Scan(&n)
	return n, err
}

func (s *Store) credentialsForUser(ctx context.Context, userID string) ([]webauthn.Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT credential_id,public_key,sign_count,transports,backup_eligible,backup_state FROM webauthn_credentials WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []webauthn.Credential
	for rows.Next() {
		var idb, pk []byte
		var transports string
		var signCount, backupEligible, backupState int
		if err := rows.Scan(&idb, &pk, &signCount, &transports, &backupEligible, &backupState); err != nil {
			return nil, err
		}
		out = append(out, webauthn.Credential{
			ID:        idb,
			PublicKey: pk,
			// BackupEligible is load-bearing: the library rejects an assertion
			// whose BE flag differs from the stored one. Omitting it would make
			// every synced passkey (BE=1) impossible to sign in with, so the
			// column must be restored alongside BackupState.
			Flags: webauthn.CredentialFlags{BackupEligible: backupEligible != 0, BackupState: backupState != 0},
			Authenticator: webauthn.Authenticator{
				SignCount: uint32(signCount),
			},
			Transport: storedTransports(transports),
		})
	}
	return out, rows.Err()
}

// storedTransports decodes the comma-joined transports column back into the
// library's type. An empty value yields nil rather than a one-element slice.
func storedTransports(value string) []protocol.AuthenticatorTransport {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]protocol.AuthenticatorTransport, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, protocol.AuthenticatorTransport(p))
		}
	}
	return out
}

func (s *Store) credentialByCredentialID(ctx context.Context, credentialID []byte) (storedCredential, error) {
	var c storedCredential
	var transports string
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,credential_id,public_key,sign_count,transports,name,backup_eligible,backup_state,attestation_type,aaguid,created_at,last_used_at FROM webauthn_credentials WHERE credential_id=?`, credentialID).
		Scan(&c.rowID, &c.userID, &c.credentialID, &c.publicKey, &c.signCount, &transports, &c.name, &c.backupEligible, &c.backupState, &c.attestationType, &c.aaguid, &c.createdAt, &c.lastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return storedCredential{}, ErrPasskeyNotFound
	}
	if err != nil {
		return storedCredential{}, err
	}
	if transports != "" {
		c.transports = strings.Split(transports, ",")
	}
	return c, nil
}

// maxPasskeyName bounds the stored label so a registration cannot persist an
// arbitrarily large name through the query string; it matches RenamePasskey.
const maxPasskeyName = 80

// addPasskey persists a finished registration. A duplicate credential id is a
// conflict, never a silent overwrite.
func (s *Store) addPasskey(ctx context.Context, userID string, cred webauthn.Credential, name string) error {
	if len(cred.ID) == 0 || len(cred.PublicKey) == 0 {
		return errors.New("identity: invalid passkey credential")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Passkey"
	}
	if len([]rune(name)) > maxPasskeyName {
		return errors.New("identity: passkey name must be 80 characters or fewer")
	}
	rowID, err := id.New()
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO webauthn_credentials(id,user_id,credential_id,public_key,attestation_type,aaguid,sign_count,transports,name,backup_eligible,backup_state,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		rowID, userID, cred.ID, cred.PublicKey, cred.AttestationType, aaguidString(cred.Authenticator.AAGUID),
		cred.Authenticator.SignCount, strings.Join(transportsToStrings(cred.Transport), ","), name,
		boolInt(cred.Flags.BackupEligible), boolInt(cred.Flags.BackupState), formatTime(time.Now().UTC()))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrAlreadyUsed
		}
		return err
	}
	return nil
}

// recordPasskeyUse advances the stored signature counter and backup state. The
// counter update is a single conditional statement so two concurrent
// assertions cannot interleave a stale read and regress the stored counter: the
// write only lands when the new count is greater, or either side is zero (the
// spec's "counter not enforced" case). It reports whether this assertion did
// not advance the counter (a possible cloned-authenticator signal), which the
// caller must log.
func (s *Store) recordPasskeyUse(ctx context.Context, credentialID []byte, signCount uint32, backupState bool) (bool, error) {
	now := formatTime(time.Now().UTC())
	res, err := s.db.ExecContext(ctx, `UPDATE webauthn_credentials SET sign_count=?,backup_state=?,last_used_at=?
		WHERE credential_id=? AND (sign_count < ? OR (? = 0 AND sign_count = 0))`,
		signCount, boolInt(backupState), now, credentialID, signCount, signCount)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		return false, nil
	}
	// Zero rows means either the credential is gone or the counter did not
	// advance. Distinguish the two so a deleted credential is not reported as
	// a clone warning.
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM webauthn_credentials WHERE credential_id=?`, credentialID).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, ErrPasskeyNotFound
	}
	_, err = s.db.ExecContext(ctx, `UPDATE webauthn_credentials SET backup_state=?,last_used_at=? WHERE credential_id=?`,
		boolInt(backupState), now, credentialID)
	return true, err
}

// RenamePasskey changes a passkey's label, scoped to the owning user.
func (s *Store) RenamePasskey(ctx context.Context, userID, rowID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("identity: passkey name is required")
	}
	if len([]rune(name)) > maxPasskeyName {
		return errors.New("identity: passkey name must be 80 characters or fewer")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE webauthn_credentials SET name=? WHERE id=? AND user_id=?`, name, rowID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPasskeyNotFound
	}
	return nil
}

// DeletePasskey removes a passkey. It refuses when doing so would leave the
// account with no sign-in method at all: no password, no Google identity, and
// no other passkey. The check and the delete run in one transaction so two
// concurrent deletes of the last two passkeys can never both pass the guard.
func (s *Store) DeletePasskey(ctx context.Context, userID, rowID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var passwordEnabled, googleLinked bool
	if err := tx.QueryRowContext(ctx, `SELECT password_auth_enabled, EXISTS(SELECT 1 FROM user_identities i WHERE i.user_id=u.id AND i.provider='google') FROM users u WHERE u.id=?`, userID).Scan(&passwordEnabled, &googleLinked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM webauthn_credentials WHERE user_id=?`, userID).Scan(&count); err != nil {
		return err
	}
	if !passwordEnabled && !googleLinked && count <= 1 {
		return ErrLastAuthMethod
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM webauthn_credentials WHERE id=? AND user_id=?`, rowID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPasskeyNotFound
	}
	// No session revocation: passkey rows are not part of the cached session
	// snapshot, and removing one credential (of several) is not a sign-out.
	return tx.Commit()
}

// SetPasswordSignInEnabled enables or disables password sign-in for a user.
// Disabling makes the account passkey-only and is refused when the user has no
// passkey. Enabling is refused while a Google identity is linked, because
// linking Google disables password auth by design and re-enabling it would
// break that invariant (and silently break password reset). It backs the "use
// this passkey as your only sign-in method" flow and the inverse.
//
// keepSession is the selector of the session making the change (may be empty).
// It is retained when disabling/enabling so the user is not signed out of the
// tab they are using; all other sessions are revoked.
func (s *Store) SetPasswordSignInEnabled(ctx context.Context, userID, keepSession string, enabled bool) error {
	// keepSession is a raw session token; the session row's primary key is its
	// selector, so extract it here. A malformed token is ignored (revoke
	// everything) rather than failing the credential change.
	keepSelector := ""
	if keepSession != "" {
		if selector, err := sessionSelector(keepSession); err == nil {
			keepSelector = selector
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var googleLinked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_identities WHERE user_id=? AND provider='google')`, userID).Scan(&googleLinked); err != nil {
		return err
	}
	if enabled && googleLinked {
		return ErrPasswordDisabled
	}
	if !enabled {
		var passkeys int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM webauthn_credentials WHERE user_id=?`, userID).Scan(&passkeys); err != nil {
			return err
		}
		if passkeys == 0 {
			return ErrLastAuthMethod
		}
	}
	// Bump the auth generation so any in-flight password login that
	// authenticated against the previous state cannot mint a session after the
	// change commits, mirroring a password change.
	res, err := tx.ExecContext(ctx, `UPDATE users SET password_auth_enabled=?,auth_generation=auth_generation+1,updated_at=? WHERE id=?`, boolInt(enabled), formatTime(time.Now().UTC()), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	// A sign-in method change revokes every other session; the initiating
	// session is retained so the caller's UI keeps working. An empty selector
	// revokes everything, matching RevokeAllUserSessions.
	if keepSelector != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id=? AND id<>?`, userID, keepSelector); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id=?`, userID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if keepSelector != "" {
		// The retained session row survives; dropping its cache entry (rather
		// than leaving it) makes the next request reload the fresh
		// password_auth_enabled value instead of a stale snapshot.
		s.invalidateUserCache(userID)
	} else {
		s.InvalidateUser(userID)
	}
	return nil
}

func aaguidString(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return ""
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hexdigits[v>>4], hexdigits[v&0x0f])
	}
	return string(out)
}

func transportsToStrings(ts []protocol.AuthenticatorTransport) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, string(t))
	}
	return out
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// ---- Ceremony entry points ----

// RegistrationOptions is handed to the browser for navigator.credentials.create.
type RegistrationOptions struct {
	ChallengeToken string                       `json:"challenge_token"`
	Options        *protocol.CredentialCreation `json:"options"`
}

// BeginPasskeyRegistration starts a registration ceremony for a signed-in user.
func (s *Store) BeginPasskeyRegistration(ctx context.Context, u User) (RegistrationOptions, error) {
	s.mu.Lock()
	wa := s.webauthn
	ch := s.challenges
	s.mu.Unlock()
	if wa == nil {
		return RegistrationOptions{}, ErrWebAuthnUnavailable
	}
	creds, err := s.credentialsForUser(ctx, u.ID)
	if err != nil {
		return RegistrationOptions{}, err
	}
	wu := &webAuthnUser{u: u, creds: creds}
	creation, session, err := wa.BeginRegistration(wu,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(webauthn.Credentials(wu.creds).CredentialDescriptors()),
	)
	if err != nil {
		return RegistrationOptions{}, err
	}
	token, ok := ch.put(challengeRegister, u.ID, *session)
	if !ok {
		return RegistrationOptions{}, errors.New("identity: too many pending passkey ceremonies")
	}
	return RegistrationOptions{ChallengeToken: token, Options: creation}, nil
}

// FinishPasskeyRegistration validates the response and stores the passkey.
func (s *Store) FinishPasskeyRegistration(ctx context.Context, u User, challengeToken, name string, r *http.Request) error {
	s.mu.Lock()
	wa := s.webauthn
	ch := s.challenges
	s.mu.Unlock()
	if wa == nil {
		return ErrWebAuthnUnavailable
	}
	userID, session, ok := ch.take(challengeToken, challengeRegister)
	if !ok || userID != u.ID {
		return ErrInvalidToken
	}
	creds, err := s.credentialsForUser(ctx, u.ID)
	if err != nil {
		return err
	}
	wu := &webAuthnUser{u: u, creds: creds}
	cred, err := wa.FinishRegistration(wu, session, r)
	if err != nil {
		return err
	}
	return s.addPasskey(ctx, u.ID, *cred, name)
}

// BeginPasskeyReauth starts an assertion ceremony scoped to one already
// signed-in user. It backs the re-authentication step of sensitive account
// operations for passkey-only accounts (and for anyone who prefers not to
// re-type their password). It uses allowCredentials rather than discoverable
// login because the user is known.
func (s *Store) BeginPasskeyReauth(ctx context.Context, u User) (LoginOptions, error) {
	s.mu.Lock()
	wa := s.webauthn
	ch := s.challenges
	s.mu.Unlock()
	if wa == nil {
		return LoginOptions{}, ErrWebAuthnUnavailable
	}
	creds, err := s.credentialsForUser(ctx, u.ID)
	if err != nil {
		return LoginOptions{}, err
	}
	if len(creds) == 0 {
		return LoginOptions{}, ErrPasskeyNotFound
	}
	wu := &webAuthnUser{u: u, creds: creds}
	assertion, session, err := wa.BeginLogin(wu)
	if err != nil {
		return LoginOptions{}, err
	}
	token, ok := ch.put(challengeReauth, u.ID, *session)
	if !ok {
		return LoginOptions{}, errors.New("identity: too many pending passkey ceremonies")
	}
	return LoginOptions{ChallengeToken: token, Options: assertion}, nil
}

// FinishPasskeyReauth validates a re-auth assertion for exactly the signed-in
// user and records the credential use. Unlike FinishPasskeyLogin it performs no
// account-state checks: the session that called it is already authenticated,
// and the caller only needs proof that the same person is still present.
func (s *Store) FinishPasskeyReauth(ctx context.Context, u User, challengeToken string, r *http.Request) error {
	s.mu.Lock()
	wa := s.webauthn
	ch := s.challenges
	s.mu.Unlock()
	if wa == nil {
		return ErrWebAuthnUnavailable
	}
	userID, session, ok := ch.take(challengeToken, challengeReauth)
	if !ok || userID != u.ID {
		return ErrInvalidToken
	}
	creds, err := s.credentialsForUser(ctx, u.ID)
	if err != nil {
		return err
	}
	wu := &webAuthnUser{u: u, creds: creds}
	cred, err := wa.FinishLogin(wu, session, r)
	if err != nil {
		return err
	}
	// The assertion is valid; a counter-bookkeeping failure must not deny the
	// re-authentication, matching FinishPasskeyLogin's contract.
	_, _ = s.recordPasskeyUse(ctx, cred.ID, cred.Authenticator.SignCount, cred.Flags.BackupState)
	return nil
}

// LoginOptions is handed to the browser for navigator.credentials.get.
type LoginOptions struct {
	ChallengeToken string                        `json:"challenge_token"`
	Options        *protocol.CredentialAssertion `json:"options"`
}

// BeginPasskeyLogin starts a usernameless, discoverable-credential login.
func (s *Store) BeginPasskeyLogin() (LoginOptions, error) {
	s.mu.Lock()
	wa := s.webauthn
	ch := s.challenges
	s.mu.Unlock()
	if wa == nil {
		return LoginOptions{}, ErrWebAuthnUnavailable
	}
	assertion, session, err := wa.BeginDiscoverableLogin()
	if err != nil {
		return LoginOptions{}, err
	}
	token, ok := ch.put(challengeLogin, "", *session)
	if !ok {
		return LoginOptions{}, errors.New("identity: too many pending passkey ceremonies")
	}
	return LoginOptions{ChallengeToken: token, Options: assertion}, nil
}

// PasskeyAssertion is the outcome of a successful passkey login. CloneWarning
// and RecordErr are diagnostics: the assertion is valid and the caller should
// log them without denying the login.
type PasskeyAssertion struct {
	User         User
	CloneWarning bool
	RecordErr    error
}

// FinishPasskeyLogin validates an assertion, resolves the owning user, records
// the use, and returns the authenticated user. The caller mints the session.
func (s *Store) FinishPasskeyLogin(ctx context.Context, challengeToken string, r *http.Request) (PasskeyAssertion, error) {
	s.mu.Lock()
	wa := s.webauthn
	ch := s.challenges
	s.mu.Unlock()
	if wa == nil {
		return PasskeyAssertion{}, ErrWebAuthnUnavailable
	}
	_, session, ok := ch.take(challengeToken, challengeLogin)
	if !ok {
		return PasskeyAssertion{}, ErrInvalidToken
	}
	var resolved User
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		cred, err := s.credentialByCredentialID(ctx, rawID)
		if err != nil {
			return nil, err
		}
		u, err := s.userByID(ctx, cred.userID)
		if err != nil {
			return nil, err
		}
		creds, err := s.credentialsForUser(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		resolved = u
		return &webAuthnUser{u: u, creds: creds}, nil
	}
	_, cred, err := wa.FinishPasskeyLogin(handler, session, r)
	if err != nil {
		return PasskeyAssertion{}, err
	}
	if resolved.Status != "active" || !resolved.Verified() {
		return PasskeyAssertion{}, ErrNotVerified
	}
	if resolved.AccountStatus != "active" {
		return PasskeyAssertion{}, ErrAccountInactive
	}
	// The assertion is valid; counter bookkeeping is diagnostic only. A
	// non-advancing counter is the spec's cloned-authenticator signal, and a
	// write failure means the counter may drift. Neither denies the login.
	cloneWarning, recordErr := s.recordPasskeyUse(ctx, cred.ID, cred.Authenticator.SignCount, cred.Flags.BackupState)
	return PasskeyAssertion{User: resolved, CloneWarning: cloneWarning, RecordErr: recordErr}, nil
}
