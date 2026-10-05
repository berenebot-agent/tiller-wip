package identity

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
)

// Local operator identity.
//
// Up to now local mode authenticated the operator with a credential fingerprint
// stored in platform_settings (auth.SessionStore) and had no users row: the
// implicit LocalAccountID account carried owner_user_id = NULL. Passkeys are
// bound to users.id, so a local install with no users row could never host one.
//
// EnsureLocalOperator fills that gap: it materialises exactly one users row for
// the local operator (synthetic <username>@local.invalid email), gives it the
// operator password, and points LocalAccountID at it via owner_user_id. The
// local session boundary (admin_sessions) is unchanged; this only unifies
// credential *storage* so both modes share the users table and passkeys work in
// both. See docs/hosted_decisions.md for the recorded decision.
//
// platform_settings keys:
//   - local_operator_user_id is the durable marker: its presence means the
//     operator row was created and every subsequent call is a no-op.
const (
	localOperatorUserIDKey = "local_operator_user_id"
	// localOperatorEmailDomain is the synthetic domain used for the operator's
	// users.email. It is never valid mail and is never sent anywhere; it only
	// satisfies users.email's UNIQUE/format constraints.
	localOperatorEmailDomain = "local.invalid"
)

// ErrLocalOperatorExists means the local account already has an owner user.
var ErrLocalOperatorExists = errors.New("identity: local operator already exists")

// LocalOperatorEmail returns the synthetic users.email for a local username.
func LocalOperatorEmail(username string) string {
	return NormalizeEmail(strings.TrimSpace(username)) + "@" + localOperatorEmailDomain
}

// LocalOperatorUserID returns the persisted local operator user id, or "" when
// no operator row exists yet.
func (s *Store) LocalOperatorUserID(ctx context.Context) (string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM platform_settings WHERE key=?`, localOperatorUserIDKey).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return userID, nil
}

// LocalOperatorUser returns the local operator user, or ErrNotFound when no
// operator row exists yet. It lets the local login/session path resolve the
// operator identity without knowing its id.
func (s *Store) LocalOperatorUser(ctx context.Context) (User, error) {
	userID, err := s.LocalOperatorUserID(ctx)
	if err != nil {
		return User{}, err
	}
	if userID == "" {
		return User{}, ErrNotFound
	}
	return s.userByID(ctx, userID)
}

// EnsureLocalOperator idempotently creates the local operator users row and
// attaches it to LocalAccountID. passwordHash is the already-argon2id-hashed
// operator credential; when empty, password is hashed instead. An empty
// passwordHash and password is allowed (the row becomes passwordless until a
// credential exists), but the common callers always supply one.
//
// It mirrors BootstrapHostedCustomer's shape: a single transaction that inserts
// the user and conditionally claims LocalAccountID's owner_user_id, so it can
// never overwrite an account that already has an owner. On a database that
// already has an operator marker it returns ErrLocalOperatorExists without
// touching anything.
func (s *Store) EnsureLocalOperator(ctx context.Context, username, password, passwordHash string) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return User{}, ErrBootstrapInvalid
	}
	if existing, err := s.LocalOperatorUserID(ctx); err != nil {
		return User{}, err
	} else if existing != "" {
		return User{}, ErrLocalOperatorExists
	}

	email := LocalOperatorEmail(username)
	if passwordHash == "" {
		if password == "" {
			return User{}, ErrBootstrapInvalid
		}
		hash, err := s.passwordHasher.Hash(username + "\x00" + password)
		if err != nil {
			return User{}, err
		}
		passwordHash = hash
	}

	userID, err := id.New()
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,email,password_hash,status,email_verified_at,password_auth_enabled,created_at,updated_at) VALUES(?,?,?,'active',?,1,?,?)`,
		userID, email, passwordHash, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return User{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE accounts SET owner_user_id=?,updated_at=? WHERE id=? AND owner_user_id IS NULL`, userID, formatTime(now), database.LocalAccountID)
	if err != nil {
		return User{}, err
	}
	if count, err := result.RowsAffected(); err != nil {
		return User{}, err
	} else if count != 1 {
		// The local account already has an owner (e.g. a hosted migration ran
		// first). Do not insert a second user; report the collision so the
		// caller can resolve rather than silently minting an orphan.
		return User{}, ErrLocalOperatorExists
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO platform_settings(key,value,updated_at) VALUES(?,?,?)`, localOperatorUserIDKey, userID, formatTime(now)); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.userByID(ctx, userID)
}

// AuthenticateLocalOperator verifies a local operator username/password against
// the operator's users row. It mirrors AuthenticatePassword but the identity is
// a synthetic email that the browser never sees; the username is what the
// operator types and must match the stored synthetic local address, so a correct
// password under a wrong username still fails. A missing operator row, disabled
// row, or unverified row all fail closed.
func (s *Store) AuthenticateLocalOperator(ctx context.Context, username, password string) (User, error) {
	userID, err := s.LocalOperatorUserID(ctx)
	if err != nil {
		return User{}, err
	}
	if userID == "" {
		return User{}, ErrNotFound
	}
	var email string
	if err := s.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id=?`, userID).Scan(&email); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	if email != LocalOperatorEmail(username) {
		return User{}, ErrNotFound
	}
	// The migrated credential hash binds username and password with a NUL
	// separator (the legacy auth.SessionStore format), so verification uses the
	// same material. A row whose hash is empty cannot authenticate.
	return s.authenticateOperatorByID(ctx, userID, username+"\x00"+password)
}

func (s *Store) authenticateOperatorByID(ctx context.Context, userID, password string) (User, error) {
	var hash string
	var enabled bool
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash,password_auth_enabled FROM users WHERE id=?`, userID).Scan(&hash, &enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	if !enabled || hash == "" {
		return User{}, ErrNotFound
	}
	if !s.passwordHasher.Verify(password, hash) {
		return User{}, ErrNotFound
	}
	u, err := s.userByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if u.Status != "active" || !u.Verified() || u.AccountStatus != "active" {
		return User{}, ErrNotFound
	}
	return u, nil
}

// SyncLocalOperatorCredentials reconciles an existing local operator users row
// with the credential the environment or the first-run wizard currently holds.
// Both the synthetic username/email and the stored password hash are updated in
// one transaction, because AuthenticateLocalOperator binds them together: a
// changed username with a stale synthetic email would reject the new username
// before password verification ever ran. auth_generation is bumped only when
// something actually changed, so a no-op boot does not churn sessions.
//
// It is a no-op when no operator row exists (the row is materialised later by
// EnsureLocalOperator). A marked row that does not own LocalAccountID is
// inconsistent and reported as ErrLocalOperatorExists rather than silently
// rewritten.
func (s *Store) SyncLocalOperatorCredentials(ctx context.Context, username, passwordHash string) error {
	username = strings.TrimSpace(username)
	if username == "" || passwordHash == "" {
		return nil
	}
	userID, err := s.LocalOperatorUserID(ctx)
	if err != nil || userID == "" {
		return err
	}
	var owner, currentEmail, currentHash string
	if err := s.db.QueryRowContext(ctx, `SELECT a.owner_user_id,u.email,u.password_hash FROM users u JOIN accounts a ON a.id=? WHERE u.id=?`, database.LocalAccountID, userID).Scan(&owner, &currentEmail, &currentHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLocalOperatorExists
		}
		return err
	}
	if owner != userID {
		return ErrLocalOperatorExists
	}
	email := LocalOperatorEmail(username)
	if currentEmail == email && currentHash == passwordHash {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE users SET email=?,password_hash=?,auth_generation=auth_generation+1,updated_at=? WHERE id=?`, email, passwordHash, formatTime(time.Now().UTC()), userID)
	return err
}
