// Package identity owns hosted human identity persistence. It deliberately
// deals only with platform-global identity tables; tenant resource SQL remains
// behind internal/store.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/id"
)

const (
	verificationTTL        = 24 * time.Hour
	resetTTL               = time.Hour
	selectorBytes          = 16
	secretBytes            = 32
	csrfBytes              = 32
	defaultSessionTTL      = 30 * 24 * time.Hour
	defaultCacheTTL        = 5 * time.Minute
	maxSessionCacheEntries = 100000
)

var (
	ErrNotFound        = errors.New("identity: not found")
	ErrInvalidToken    = errors.New("identity: invalid token")
	ErrExpiredToken    = errors.New("identity: expired token")
	ErrAlreadyUsed     = errors.New("identity: token already used")
	ErrNotVerified     = errors.New("identity: email not verified")
	ErrUserDisabled    = errors.New("identity: user disabled")
	ErrAccountInactive = errors.New("identity: account inactive")
	ErrWeakPassword    = errors.New("identity: password does not meet minimum length")
	ErrPasswordTooLong = errors.New("identity: password exceeds maximum length")
)

// User is the authenticated hosted identity and its one owned account.
type User struct {
	ID            string
	Email         string
	Status        string
	AccountID     string
	AccountStatus string
	VerifiedAt    sql.NullString
}

func (u User) Verified() bool { return u.VerifiedAt.Valid && u.VerifiedAt.String != "" }

// SignupResult contains the pending identity and the raw one-time verification
// token. The raw token is returned only to the mailer and is never persisted.
type SignupResult struct {
	User              User
	VerificationToken string
}

// UserSession is the authenticated customer session. Token is populated only
// when a session is newly created; persisted rows hold only its hash.
type UserSession struct {
	Token     string
	CSRFToken string
	ExpiresAt time.Time
	User      User
}

// PlatformSession is the separate hosted operator session.
type PlatformSession struct {
	Token     string
	CSRFToken string
	ExpiresAt time.Time
}

// PlatformUserRow is the non-secret operator view of a hosted user/account.
type PlatformUserRow struct {
	UserID        string `json:"user_id"`
	Email         string `json:"email"`
	UserStatus    string `json:"user_status"`
	Verified      bool   `json:"verified"`
	AccountID     string `json:"account_id"`
	AccountStatus string `json:"account_status"`
	Plan          string `json:"plan"`
	CreatedAt     string `json:"created_at"`
}

// Store owns hosted identity and platform-admin persistence.
type Store struct {
	db                 *sql.DB
	passwordHasher     auth.SecretHasher
	tokenHasher        auth.SecretHasher
	credentialHasher   auth.SecretHasher
	userSessionTTL     time.Duration
	platformSessionTTL time.Duration
	userCacheTTL       time.Duration
	platformCacheTTL   time.Duration
	mu                 sync.Mutex
	userCache          map[string]userSessionCacheEntry
	platformCache      map[string]platformSessionCacheEntry
	rev                uint64
	platformRev        uint64
	maxEntries         int
}

type userSessionCacheEntry struct {
	session    UserSession
	secretHash [32]byte
	expires    time.Time
}

type platformSessionCacheEntry struct {
	session    PlatformSession
	secretHash [32]byte
	expires    time.Time
}

// New constructs an identity store. Production defaults are Argon2id for human
// passwords/credentials and bcrypt for generated session tokens. Tests may
// inject fast implementations without changing persisted formats.
func New(db *sql.DB, passwordHasher, tokenHasher, credentialHasher auth.SecretHasher, userSessionTTL time.Duration) (*Store, error) {
	if db == nil {
		return nil, errors.New("identity: nil database")
	}
	if passwordHasher == nil {
		passwordHasher = auth.Argon2Hasher{}
	}
	if tokenHasher == nil {
		tokenHasher = auth.BcryptHasher{}
	}
	if credentialHasher == nil {
		credentialHasher = auth.Argon2Hasher{}
	}
	if userSessionTTL <= 0 {
		userSessionTTL = defaultSessionTTL
	}
	return &Store{
		db: db, passwordHasher: passwordHasher, tokenHasher: tokenHasher,
		credentialHasher: credentialHasher, userSessionTTL: userSessionTTL,
		platformSessionTTL: userSessionTTL, userCacheTTL: defaultCacheTTL,
		platformCacheTTL: defaultCacheTTL, userCache: make(map[string]userSessionCacheEntry),
		platformCache: make(map[string]platformSessionCacheEntry), maxEntries: maxSessionCacheEntries,
	}, nil
}

// SetCacheTTL sets both identity-session cache windows. It is intended for
// construction-time configuration and tests.
func (s *Store) SetCacheTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.userCacheTTL, s.platformCacheTTL = d, d
	s.mu.Unlock()
}

func (s *Store) ListUsers(ctx context.Context, search string, limit, offset int) ([]PlatformUserRow, error) {
	pattern := "%" + strings.TrimSpace(search) + "%"
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.email,u.status,u.email_verified_at,a.id,a.status,a.plan,u.created_at FROM users u JOIN accounts a ON a.owner_user_id=u.id WHERE u.email LIKE ? OR u.id LIKE ? OR a.id LIKE ? ORDER BY u.created_at DESC LIMIT ? OFFSET ?`, pattern, pattern, pattern, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlatformUserRow
	for rows.Next() {
		var row PlatformUserRow
		var verified sql.NullString
		if err := rows.Scan(&row.UserID, &row.Email, &row.UserStatus, &verified, &row.AccountID, &row.AccountStatus, &row.Plan, &row.CreatedAt); err != nil {
			return nil, err
		}
		row.Verified = verified.Valid && verified.String != ""
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) SetAccountStatus(ctx context.Context, accountID, status string) error {
	if status != "active" && status != "suspended" && status != "deleting" {
		return errors.New("identity: invalid account status")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET status=?,updated_at=? WHERE id=?`, status, formatTime(time.Now()), accountID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	if status != "active" {
		s.InvalidateAccount(accountID)
	}
	return nil
}

func (s *Store) AccountStatus(ctx context.Context, accountID string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM accounts WHERE id=?`, accountID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return status, err
}

// DeleteAccountIdentity removes platform identity rows after tenant resources
// have been deleted through internal/store. Audit rows deliberately remain in
// the core database (account_audit_events has no FK to accounts).
func (s *Store) DeleteAccountIdentity(ctx context.Context, accountID string) error {
	var userID string
	if err := s.db.QueryRowContext(ctx, `SELECT owner_user_id FROM accounts WHERE id=?`, accountID).Scan(&userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE account_id=?`, accountID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_verification_tokens WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM password_reset_tokens WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, accountID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.InvalidateAccount(accountID)
	return nil
}

// NormalizeEmail returns the canonical hosted identity form.
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// ValidatePassword enforces the bounded human-password policy.
func ValidatePassword(password string) error {
	if len([]byte(password)) < 12 {
		return ErrWeakPassword
	}
	if len([]byte(password)) > 1024 {
		return ErrPasswordTooLong
	}
	return nil
}

// CreateSignup creates the user, pending account, and verification token in a
// short transaction. The caller sends the returned token only after this
// transaction commits.
func (s *Store) CreateSignup(ctx context.Context, email, password string) (SignupResult, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return SignupResult{}, ErrNotFound
	}
	if err := ValidatePassword(password); err != nil {
		return SignupResult{}, err
	}
	passwordHash, err := s.passwordHasher.Hash(password)
	if err != nil {
		return SignupResult{}, err
	}
	userID, err := id.New()
	if err != nil {
		return SignupResult{}, err
	}
	accountID, err := id.New()
	if err != nil {
		return SignupResult{}, err
	}
	rawToken, selector, tokenHash, err := newOpaqueToken(s.tokenHasher)
	if err != nil {
		return SignupResult{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SignupResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,email,password_hash,status,created_at,updated_at) VALUES(?,?,?,'active',?,?)`, userID, email, passwordHash, formatTime(now), formatTime(now)); err != nil {
		return SignupResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(id,plan,status,owner_user_id,created_at,updated_at) VALUES(?,'free','pending',?,?,?)`, accountID, userID, formatTime(now), formatTime(now)); err != nil {
		return SignupResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO email_verification_tokens(id,user_id,token_hash,created_at,expires_at) VALUES(?,?,?,?,?)`, selector, userID, tokenHash, formatTime(now), formatTime(now.Add(verificationTTL))); err != nil {
		return SignupResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return SignupResult{}, err
	}
	return SignupResult{User: User{ID: userID, Email: email, Status: "active", AccountID: accountID, AccountStatus: "pending"}, VerificationToken: rawToken}, nil
}

// UserByEmail returns a user and its one owned account. It intentionally does
// not reveal whether the caller should use the result to an HTTP client.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return s.userByEmail(ctx, NormalizeEmail(email))
}

func (s *Store) userByEmail(ctx context.Context, email string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.email,u.status,u.email_verified_at,a.id,a.status FROM users u JOIN accounts a ON a.owner_user_id=u.id WHERE u.email=?`, email).
		Scan(&u.ID, &u.Email, &u.Status, &u.VerifiedAt, &u.AccountID, &u.AccountStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// AuthenticatePassword verifies an email/password and returns an active,
// verified user. The handler should map every failure to a generic response.
func (s *Store) AuthenticatePassword(ctx context.Context, email, password string) (User, error) {
	u, err := s.userByEmail(ctx, NormalizeEmail(email))
	if err != nil {
		return User{}, err
	}
	var passwordHash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, u.ID).Scan(&passwordHash); err != nil {
		return User{}, err
	}
	if !s.passwordHasher.Verify(password, passwordHash) {
		return User{}, ErrNotFound
	}
	if u.Status != "active" {
		return User{}, ErrUserDisabled
	}
	if !u.Verified() {
		return User{}, ErrNotVerified
	}
	if u.AccountStatus != "active" {
		return User{}, ErrAccountInactive
	}
	return u, nil
}

// IssueVerification replaces prior verification tokens and returns a new raw
// token for a known, unverified user. Missing/already-verified users return
// (zero, nil) so callers can keep responses enumeration-resistant.
func (s *Store) IssueVerification(ctx context.Context, email string) (User, string, error) {
	u, err := s.userByEmail(ctx, NormalizeEmail(email))
	if errors.Is(err, ErrNotFound) {
		return User{}, "", nil
	}
	if err != nil || u.Verified() {
		return u, "", err
	}
	raw, selector, hash, err := newOpaqueToken(s.tokenHasher)
	if err != nil {
		return User{}, "", err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_verification_tokens WHERE user_id=?`, u.ID); err != nil {
		return User{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO email_verification_tokens(id,user_id,token_hash,created_at,expires_at) VALUES(?,?,?,?,?)`, selector, u.ID, hash, formatTime(now), formatTime(now.Add(verificationTTL))); err != nil {
		return User{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return User{}, "", err
	}
	return u, raw, nil
}

// ConsumeVerification atomically consumes a valid verification token and
// activates the owned account.
func (s *Store) ConsumeVerification(ctx context.Context, raw string) (User, error) {
	selector, secret, ok := parseOpaqueToken(raw)
	if !ok {
		return User{}, ErrInvalidToken
	}
	var userID, hash, expires string
	if err := s.db.QueryRowContext(ctx, `SELECT user_id,token_hash,expires_at FROM email_verification_tokens WHERE id=? AND used_at IS NULL`, selector).Scan(&userID, &hash, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrInvalidToken
		}
		return User{}, err
	}
	if !s.tokenHasher.Verify(secret, hash) {
		return User{}, ErrInvalidToken
	}
	exp, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !time.Now().Before(exp) {
		return User{}, ErrExpiredToken
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE email_verification_tokens SET used_at=? WHERE id=? AND used_at IS NULL`, formatTime(now), selector)
	if err != nil {
		return User{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return User{}, ErrAlreadyUsed
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET email_verified_at=?,updated_at=? WHERE id=?`, formatTime(now), formatTime(now), userID); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET status='active',updated_at=? WHERE owner_user_id=? AND status='pending'`, formatTime(now), userID); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.userByID(ctx, userID)
}

// IssuePasswordReset replaces prior reset tokens for a known user. Missing
// users return no token without an error for enumeration-resistant handlers.
func (s *Store) IssuePasswordReset(ctx context.Context, email string) (User, string, error) {
	u, err := s.userByEmail(ctx, NormalizeEmail(email))
	if errors.Is(err, ErrNotFound) {
		return User{}, "", nil
	}
	if err != nil {
		return User{}, "", err
	}
	raw, selector, hash, err := newOpaqueToken(s.tokenHasher)
	if err != nil {
		return User{}, "", err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM password_reset_tokens WHERE user_id=?`, u.ID); err != nil {
		return User{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO password_reset_tokens(id,user_id,token_hash,created_at,expires_at) VALUES(?,?,?,?,?)`, selector, u.ID, hash, formatTime(now), formatTime(now.Add(resetTTL))); err != nil {
		return User{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return User{}, "", err
	}
	return u, raw, nil
}

// ConsumePasswordReset atomically changes the password, consumes the token,
// and revokes every customer session owned by the user.
func (s *Store) ConsumePasswordReset(ctx context.Context, raw, password string) (User, error) {
	if err := ValidatePassword(password); err != nil {
		return User{}, err
	}
	selector, secret, ok := parseOpaqueToken(raw)
	if !ok {
		return User{}, ErrInvalidToken
	}
	var userID, hash, expires string
	if err := s.db.QueryRowContext(ctx, `SELECT user_id,token_hash,expires_at FROM password_reset_tokens WHERE id=? AND used_at IS NULL`, selector).Scan(&userID, &hash, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrInvalidToken
		}
		return User{}, err
	}
	if !s.tokenHasher.Verify(secret, hash) {
		return User{}, ErrInvalidToken
	}
	exp, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !time.Now().Before(exp) {
		return User{}, ErrExpiredToken
	}
	newHash, err := s.passwordHasher.Hash(password)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE password_reset_tokens SET used_at=? WHERE id=? AND used_at IS NULL`, formatTime(now), selector)
	if err != nil {
		return User{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return User{}, ErrAlreadyUsed
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?,updated_at=? WHERE id=?`, newHash, formatTime(now), userID); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id=?`, userID); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	s.InvalidateUser(userID)
	return s.userByID(ctx, userID)
}

// CreateUserSession creates a server-side customer session for an active,
// verified account.
func (s *Store) CreateUserSession(ctx context.Context, u User) (UserSession, error) {
	if u.Status != "active" || !u.Verified() {
		return UserSession{}, ErrNotVerified
	}
	if u.AccountStatus != "active" {
		return UserSession{}, ErrAccountInactive
	}
	raw, selector, hash, err := newOpaqueToken(s.tokenHasher)
	if err != nil {
		return UserSession{}, err
	}
	csrf, err := randomURL(csrfBytes)
	if err != nil {
		return UserSession{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(s.userSessionTTL)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO user_sessions(id,user_id,account_id,token_hash,csrf_token,created_at,expires_at,last_used_at) VALUES(?,?,?,?,?,?,?,?)`, selector, u.ID, u.AccountID, hash, csrf, formatTime(now), formatTime(expires), formatTime(now)); err != nil {
		return UserSession{}, err
	}
	return UserSession{Token: raw, CSRFToken: csrf, ExpiresAt: expires, User: u}, nil
}

// GetUserSession validates a customer session and applies sliding expiry.
func (s *Store) GetUserSession(ctx context.Context, raw string) (UserSession, bool) {
	selector, secret, ok := parseOpaqueToken(raw)
	if !ok {
		return UserSession{}, false
	}
	now := time.Now()
	s.mu.Lock()
	if entry, found := s.userCache[selector]; found {
		if now.Before(entry.expires) && now.Before(entry.session.ExpiresAt) && constantSecret(secret, entry.secretHash) {
			entry.expires = now.Add(s.userCacheTTL)
			s.userCache[selector] = entry
			s.mu.Unlock()
			return entry.session, true
		}
		delete(s.userCache, selector)
	}
	s.mu.Unlock()

	generation := atomic.LoadUint64(&s.rev)
	var session UserSession
	var hash, expires string
	var status, accountStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT us.csrf_token,us.token_hash,us.expires_at,u.id,u.email,u.status,u.email_verified_at,us.account_id,a.status FROM user_sessions us JOIN users u ON u.id=us.user_id JOIN accounts a ON a.id=us.account_id WHERE us.id=?`, selector).
		Scan(&session.CSRFToken, &hash, &expires, &session.User.ID, &session.User.Email, &status, &session.User.VerifiedAt, &session.User.AccountID, &accountStatus); err != nil {
		return UserSession{}, false
	}
	session.User.Status, session.User.AccountStatus = status, accountStatus
	exp, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !now.Before(exp) || status != "active" || !session.User.Verified() || accountStatus != "active" || !s.tokenHasher.Verify(secret, hash) {
		return UserSession{}, false
	}
	if now.Add(s.userSessionTTL / 2).After(exp) {
		exp = now.Add(s.userSessionTTL)
		_, _ = s.db.ExecContext(ctx, `UPDATE user_sessions SET expires_at=?,last_used_at=? WHERE id=?`, formatTime(exp), formatTime(now), selector)
	}
	session.ExpiresAt = exp
	s.mu.Lock()
	defer s.mu.Unlock()
	if atomic.LoadUint64(&s.rev) != generation {
		return UserSession{}, false
	}
	s.ensureUserCacheRoomLocked(now)
	s.userCache[selector] = userSessionCacheEntry{session: session, expires: now.Add(s.userCacheTTL), secretHash: sha256.Sum256([]byte(secret))}
	return session, true
}

func (s *Store) DeleteUserSession(raw string) {
	selector, _, ok := parseOpaqueToken(raw)
	if !ok {
		return
	}
	s.mu.Lock()
	_, _ = s.db.Exec(`DELETE FROM user_sessions WHERE id=?`, selector)
	atomic.AddUint64(&s.rev, 1)
	delete(s.userCache, selector)
	s.mu.Unlock()
}

func (s *Store) CheckUserCSRF(session UserSession, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(session.CSRFToken), []byte(token)) == 1
}

// InvalidateUser and InvalidateAccount are immediate revocation paths used by
// password reset, user disablement, and account suspension/deletion.
func (s *Store) InvalidateUser(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`DELETE FROM user_sessions WHERE user_id=?`, userID)
	atomic.AddUint64(&s.rev, 1)
	for selector, entry := range s.userCache {
		if entry.session.User.ID == userID {
			delete(s.userCache, selector)
		}
	}
}

func (s *Store) InvalidateAccount(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`DELETE FROM user_sessions WHERE account_id=?`, accountID)
	atomic.AddUint64(&s.rev, 1)
	for selector, entry := range s.userCache {
		if entry.session.User.AccountID == accountID {
			delete(s.userCache, selector)
		}
	}
}

// StartSweeper removes expired cached customer and platform sessions.
func (s *Store) StartSweeper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.SweepExpired()
			}
		}
	}()
}

func (s *Store) SweepExpired() {
	now := time.Now()
	s.mu.Lock()
	for selector, entry := range s.userCache {
		if !now.Before(entry.expires) || !now.Before(entry.session.ExpiresAt) {
			delete(s.userCache, selector)
		}
	}
	for selector, entry := range s.platformCache {
		if !now.Before(entry.expires) || !now.Before(entry.session.ExpiresAt) {
			delete(s.platformCache, selector)
		}
	}
	s.mu.Unlock()
}

// SyncPlatformCredential stores a fingerprint of the hosted platform
// credentials and revokes platform sessions when they change.
func (s *Store) SyncPlatformCredential(username, password string) error {
	material := username + "\x00" + password
	var stored string
	err := s.db.QueryRow(`SELECT value FROM platform_settings WHERE key='admin_credential_hash'`).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		hash, err := s.credentialHasher.Hash(material)
		if err != nil {
			return err
		}
		_, err = s.db.Exec(`INSERT INTO platform_settings(key,value,updated_at) VALUES('admin_credential_hash',?,?)`, hash, formatTime(time.Now()))
		return err
	}
	if err != nil {
		return err
	}
	if s.credentialHasher.Verify(material, stored) {
		return nil
	}
	if err := s.InvalidatePlatformAll(); err != nil {
		return err
	}
	hash, err := s.credentialHasher.Hash(material)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE platform_settings SET value=?,updated_at=? WHERE key='admin_credential_hash'`, hash, formatTime(time.Now()))
	return err
}

// AuthenticatePlatform checks the configured platform username/password. The
// caller should map all failures to one generic operator-authentication error.
func (s *Store) AuthenticatePlatform(username, password, configuredUsername, configuredPassword string) bool {
	return auth.EqualCredential(username, configuredUsername) && auth.EqualCredential(password, configuredPassword)
}

func (s *Store) CreatePlatformSession(ctx context.Context) (PlatformSession, error) {
	raw, selector, hash, err := newOpaqueToken(s.tokenHasher)
	if err != nil {
		return PlatformSession{}, err
	}
	csrf, err := randomURL(csrfBytes)
	if err != nil {
		return PlatformSession{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(s.platformSessionTTL)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO platform_admin_sessions(id,token_hash,csrf_token,created_at,expires_at,last_used_at) VALUES(?,?,?,?,?,?)`, selector, hash, csrf, formatTime(now), formatTime(expires), formatTime(now)); err != nil {
		return PlatformSession{}, err
	}
	return PlatformSession{Token: raw, CSRFToken: csrf, ExpiresAt: expires}, nil
}

func (s *Store) GetPlatformSession(ctx context.Context, raw string) (PlatformSession, bool) {
	selector, secret, ok := parseOpaqueToken(raw)
	if !ok {
		return PlatformSession{}, false
	}
	now := time.Now()
	s.mu.Lock()
	if entry, found := s.platformCache[selector]; found {
		if now.Before(entry.expires) && now.Before(entry.session.ExpiresAt) && constantSecret(secret, entry.secretHash) {
			entry.expires = now.Add(s.platformCacheTTL)
			s.platformCache[selector] = entry
			s.mu.Unlock()
			return entry.session, true
		}
		delete(s.platformCache, selector)
	}
	s.mu.Unlock()

	generation := atomic.LoadUint64(&s.platformRev)
	var session PlatformSession
	var hash, expires string
	if err := s.db.QueryRowContext(ctx, `SELECT csrf_token,token_hash,expires_at FROM platform_admin_sessions WHERE id=?`, selector).Scan(&session.CSRFToken, &hash, &expires); err != nil {
		return PlatformSession{}, false
	}
	exp, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !now.Before(exp) || !s.tokenHasher.Verify(secret, hash) {
		return PlatformSession{}, false
	}
	if now.Add(s.platformSessionTTL / 2).After(exp) {
		exp = now.Add(s.platformSessionTTL)
		_, _ = s.db.ExecContext(ctx, `UPDATE platform_admin_sessions SET expires_at=?,last_used_at=? WHERE id=?`, formatTime(exp), formatTime(now), selector)
	}
	session.ExpiresAt = exp
	s.mu.Lock()
	defer s.mu.Unlock()
	if atomic.LoadUint64(&s.platformRev) != generation {
		return PlatformSession{}, false
	}
	s.ensurePlatformCacheRoomLocked(now)
	s.platformCache[selector] = platformSessionCacheEntry{session: session, expires: now.Add(s.platformCacheTTL), secretHash: sha256.Sum256([]byte(secret))}
	return session, true
}

func (s *Store) DeletePlatformSession(raw string) {
	selector, _, ok := parseOpaqueToken(raw)
	if !ok {
		return
	}
	s.mu.Lock()
	_, _ = s.db.Exec(`DELETE FROM platform_admin_sessions WHERE id=?`, selector)
	atomic.AddUint64(&s.platformRev, 1)
	delete(s.platformCache, selector)
	s.mu.Unlock()
}

func (s *Store) CheckPlatformCSRF(session PlatformSession, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(session.CSRFToken), []byte(token)) == 1
}

func (s *Store) InvalidatePlatformAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM platform_admin_sessions`)
	atomic.AddUint64(&s.platformRev, 1)
	s.platformCache = make(map[string]platformSessionCacheEntry)
	return err
}

func (s *Store) ensureUserCacheRoomLocked(now time.Time) {
	if len(s.userCache) < s.maxEntries {
		return
	}
	for selector, entry := range s.userCache {
		if !now.Before(entry.expires) || !now.Before(entry.session.ExpiresAt) {
			delete(s.userCache, selector)
		}
	}
	for len(s.userCache) >= s.maxEntries {
		for selector := range s.userCache {
			delete(s.userCache, selector)
			break
		}
	}
}

func (s *Store) ensurePlatformCacheRoomLocked(now time.Time) {
	if len(s.platformCache) < s.maxEntries {
		return
	}
	for selector, entry := range s.platformCache {
		if !now.Before(entry.expires) || !now.Before(entry.session.ExpiresAt) {
			delete(s.platformCache, selector)
		}
	}
	for len(s.platformCache) >= s.maxEntries {
		for selector := range s.platformCache {
			delete(s.platformCache, selector)
			break
		}
	}
}

func (s *Store) userByID(ctx context.Context, userID string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.email,u.status,u.email_verified_at,a.id,a.status FROM users u JOIN accounts a ON a.owner_user_id=u.id WHERE u.id=?`, userID).
		Scan(&u.ID, &u.Email, &u.Status, &u.VerifiedAt, &u.AccountID, &u.AccountStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func newOpaqueToken(hasher auth.SecretHasher) (raw, selector, hash string, err error) {
	selectorRaw, err := randomBytes(selectorBytes)
	if err != nil {
		return "", "", "", err
	}
	secretRaw, err := randomBytes(secretBytes)
	if err != nil {
		return "", "", "", err
	}
	selector = base64.RawURLEncoding.EncodeToString(selectorRaw)
	secret := base64.RawURLEncoding.EncodeToString(secretRaw)
	hash, err = hasher.Hash(secret)
	if err != nil {
		return "", "", "", err
	}
	return selector + "." + secret, selector, hash, nil
}

func parseOpaqueToken(raw string) (selector, secret string, ok bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return "", "", false
	}
	selectorRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(selectorRaw) != selectorBytes {
		return "", "", false
	}
	secretBytesDecoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(secretBytesDecoded) != secretBytes {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func randomURL(n int) (string, error) {
	b, err := randomBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return b, err
}

func constantSecret(secret string, want [32]byte) bool {
	got := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
