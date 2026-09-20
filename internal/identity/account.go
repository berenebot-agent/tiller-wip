package identity

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"time"

	"github.com/tiller-router/tiller-router/internal/crypto"
	"github.com/tiller-router/tiller-router/internal/mailoutbox"
)

// Account-level errors. Handlers deliberately map most to a generic response to
// stay enumeration-resistant.
var (
	// ErrEmailUnchanged is returned when the requested new address equals the
	// current one.
	ErrEmailUnchanged = errors.New("identity: email is unchanged")
	// ErrEmailTaken is returned internally when the target address already
	// belongs to another user. The request handler keeps its external response
	// generic and sends no mail.
	ErrEmailTaken = errors.New("identity: email is already registered")
	// ErrInvalidSession is returned when a raw session token cannot be parsed.
	ErrInvalidSession = errors.New("identity: invalid session token")
	// ErrMailUnavailable is returned when a queued message cannot be encrypted
	// because the master key is locked.
	ErrMailUnavailable = errors.New("identity: mail unavailable while the master key is locked")
)

// MailQueue is the durable-mail dependency identity flows enqueue into. It must
// be called inside the transaction that creates the one-time token; Nudge runs
// only after that transaction commits.
type MailQueue interface {
	Enqueue(ctx context.Context, tx *sql.Tx, msg mailoutbox.QueuedMessage) error
	Nudge()
}

// SetMailQueue installs the durable mail queue. When unset (tests, local mode)
// identity flows still create tokens and return the raw token to the caller.
func (s *Store) SetMailQueue(q MailQueue) { s.mailQueue = q }

// AccountProfile is the non-secret account view rendered by the Account page.
type AccountProfile struct {
	UserID        string `json:"user_id"`
	Email         string `json:"email"`
	AccountID     string `json:"account_id"`
	Plan          string `json:"plan"`
	UserStatus    string `json:"user_status"`
	AccountStatus string `json:"account_status"`
	Verified      bool   `json:"verified"`
	CreatedAt     string `json:"created_at"`
}

// sessionSelector extracts the selector from a raw session token. The selector
// is the non-secret half used as the server-side primary key.
func sessionSelector(raw string) (string, error) {
	selector, _, ok := parseOpaqueToken(raw)
	if !ok {
		return "", ErrInvalidSession
	}
	return selector, nil
}

// VerifyPassword re-authenticates an already-authenticated user. It is used
// before sensitive account changes (password, email, delete).
func (s *Store) VerifyPassword(ctx context.Context, userID, password string) error {
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, userID).Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !s.passwordHasher.Verify(password, hash) {
		return ErrNotFound
	}
	return nil
}

// AccountProfile returns the account view for an authenticated user.
func (s *Store) AccountProfile(ctx context.Context, userID string) (AccountProfile, error) {
	var p AccountProfile
	var verified sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.email,u.status,u.email_verified_at,a.id,a.plan,a.status,u.created_at FROM users u JOIN accounts a ON a.owner_user_id=u.id WHERE u.id=?`, userID).
		Scan(&p.UserID, &p.Email, &p.UserStatus, &verified, &p.AccountID, &p.Plan, &p.AccountStatus, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountProfile{}, ErrNotFound
	}
	if err != nil {
		return AccountProfile{}, err
	}
	p.Verified = verified.Valid && verified.String != ""
	return p, nil
}

// ChangePassword verifies the current password, stores a new hash, revokes
// every session except the initiating one, and cancels any pending email
// change. currentSession is the raw session token from the request cookie.
func (s *Store) ChangePassword(ctx context.Context, userID, currentSession, currentPassword, newPassword string) (User, error) {
	if err := ValidatePassword(newPassword); err != nil {
		return User{}, err
	}
	keepSelector, err := sessionSelector(currentSession)
	if err != nil {
		return User{}, err
	}
	if err := s.VerifyPassword(ctx, userID, currentPassword); err != nil {
		return User{}, err
	}
	newHash, err := s.passwordHasher.Hash(newPassword)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?,updated_at=? WHERE id=?`, newHash, formatTime(now), userID); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id=? AND id<>?`, userID, keepSelector); err != nil {
		return User{}, err
	}
	// A pending email change must not survive a password change: the warning
	// mail tells the owner to reset their password to cancel it, and this is
	// where that promise is kept.
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_change_tokens WHERE user_id=?`, userID); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	s.invalidateOtherUserSessions(userID, keepSelector)
	return s.userByID(ctx, userID)
}

// RevokeAllUserSessions deletes every session for the user, including the
// initiating one, and clears the cache so revocation is immediate.
func (s *Store) RevokeAllUserSessions(ctx context.Context, userID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id=?`, userID); err != nil {
		return err
	}
	s.InvalidateUser(userID)
	return nil
}

// RequestEmailChange validates a new address and creates a confirmation token.
// It enqueues a confirmation message to the new address and a warning to the
// current one in the same transaction. When the target address already belongs
// to another user it returns ErrEmailTaken without enqueuing any mail, so the
// endpoint cannot be used to send unsolicited messages to another customer.
func (s *Store) RequestEmailChange(ctx context.Context, userID, currentSession, newEmail string) (string, error) {
	newEmail = NormalizeEmail(newEmail)
	if !ValidateEmail(newEmail) {
		return "", ErrNotFound
	}
	keepSelector, err := sessionSelector(currentSession)
	if err != nil {
		return "", err
	}
	current, err := s.userByID(ctx, userID)
	if err != nil {
		return "", err
	}
	if NormalizeEmail(current.Email) == newEmail {
		return "", ErrEmailUnchanged
	}
	var taken int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE email=? AND id<>?`, newEmail, userID).Scan(&taken); err != nil {
		return "", err
	}
	if taken != 0 {
		return "", ErrEmailTaken
	}
	raw, selector, hash, err := newOpaqueToken(s.tokenHasher)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_change_tokens WHERE user_id=?`, userID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO email_change_tokens(id,user_id,new_email,token_hash,keep_session_id,created_at,expires_at) VALUES(?,?,?,?,?,?,?)`, selector, userID, newEmail, hash, keepSelector, formatTime(now), formatTime(now.Add(emailChangeTTL))); err != nil {
		return "", err
	}
	if err := s.enqueueMail(ctx, tx, mailoutbox.QueuedMessage{UserID: userID, Type: mailoutbox.TypeEmailChangeConfirm, Recipient: newEmail, Token: raw}); err != nil {
		return "", err
	}
	if err := s.enqueueMail(ctx, tx, mailoutbox.QueuedMessage{UserID: userID, Type: mailoutbox.TypeEmailChangeWarning, Recipient: current.Email, Params: map[string]string{"new_email": newEmail}}); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if s.mailQueue != nil {
		s.mailQueue.Nudge()
	}
	return raw, nil
}

// ConfirmEmailChange consumes a confirmation token, re-checks uniqueness to
// close the request-time race, updates the login identity, and revokes every
// session except the one that initiated the change.
func (s *Store) ConfirmEmailChange(ctx context.Context, rawToken string) (User, error) {
	selector, secret, ok := parseOpaqueToken(rawToken)
	if !ok {
		return User{}, ErrInvalidToken
	}
	var userID, newEmail, hash, expires string
	var keep sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT user_id,new_email,token_hash,expires_at,keep_session_id FROM email_change_tokens WHERE id=? AND used_at IS NULL`, selector).
		Scan(&userID, &newEmail, &hash, &expires, &keep); err != nil {
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
	var taken int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE email=? AND id<>?`, newEmail, userID).Scan(&taken); err != nil {
		return User{}, err
	}
	if taken != 0 {
		return User{}, ErrEmailTaken
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE email_change_tokens SET used_at=? WHERE id=? AND used_at IS NULL`, formatTime(now), selector)
	if err != nil {
		return User{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return User{}, ErrAlreadyUsed
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET email=?,email_verified_at=?,updated_at=? WHERE id=?`, newEmail, formatTime(now), formatTime(now), userID); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_change_tokens WHERE user_id=?`, userID); err != nil {
		return User{}, err
	}
	if keep.Valid && keep.String != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id=? AND id<>?`, userID, keep.String); err != nil {
			return User{}, err
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id=?`, userID); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	// Drop the whole user's cache so the retained session reloads the new email
	// on its next request instead of serving the stale address. This must not
	// touch the database: the retained session row deliberately survives.
	s.invalidateUserCache(userID)
	return s.userByID(ctx, userID)
}

// invalidateUserCache clears cached sessions for a user and bumps the
// generation without deleting any session rows. It is used when sessions are
// changed directly (email-change confirm) rather than revoked wholesale.
func (s *Store) invalidateUserCache(userID string) {
	s.mu.Lock()
	atomic.AddUint64(&s.rev, 1)
	for selector, entry := range s.userCache {
		if entry.session.User.ID == userID {
			delete(s.userCache, selector)
		}
	}
	s.mu.Unlock()
}

// enqueueMail is a no-op when no queue is installed (tests and local mode).
func (s *Store) enqueueMail(ctx context.Context, tx *sql.Tx, msg mailoutbox.QueuedMessage) error {
	if s.mailQueue == nil {
		return nil
	}
	if err := s.mailQueue.Enqueue(ctx, tx, msg); err != nil {
		if errors.Is(err, crypto.ErrNoKey) {
			return ErrMailUnavailable
		}
		return err
	}
	return nil
}

// nudgeMail wakes the worker after a committed enqueue. It is a no-op when no
// queue is installed.
func (s *Store) nudgeMail() {
	if s.mailQueue != nil {
		s.mailQueue.Nudge()
	}
}

// invalidateOtherUserSessions drops cached sessions for the user except the
// retained selector and bumps the generation so concurrent readers fail closed.
func (s *Store) invalidateOtherUserSessions(userID, keepSelector string) {
	s.mu.Lock()
	atomic.AddUint64(&s.rev, 1)
	for selector, entry := range s.userCache {
		if entry.session.User.ID == userID && selector != keepSelector {
			delete(s.userCache, selector)
		}
	}
	s.mu.Unlock()
}

// PruneExpiredTokens removes used or expired one-time identity tokens. The rows
// are hash-only, so this is a size-control measure, not a security one. It is
// run by the scheduled maintenance pass.
func (s *Store) PruneExpiredTokens(ctx context.Context, now time.Time) error {
	cutoff := formatTime(now.UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"email_verification_tokens", "password_reset_tokens", "email_change_tokens"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE expires_at < ? OR (used_at IS NOT NULL AND used_at < ?)`, cutoff, cutoff); err != nil {
			return err
		}
	}
	return tx.Commit()
}
