// Package mailoutbox is the durable transactional-mail boundary. Identity
// flows enqueue a message inside the same SQLite transaction that creates the
// one-time token, so a crash can never leave a token without a queued delivery.
// A single background worker delivers with bounded retries.
//
// The raw one-time token is a recoverable secret: it is encrypted at rest with
// the always-on master key (internal/crypto, enc:v1:) and scrubbed from the row
// on successful send. Recipient and params stay readable for operator triage.
//
// This package deliberately depends only on stdlib, internal/crypto, and
// internal/mailer. It owns its own table SQL and never imports internal/store,
// so dead-letter auditing is injected as a callback rather than reached for.
package mailoutbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tiller-router/tiller-router/internal/crypto"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/mailer"
)

// Message types. Message copy is rendered here at send time from the type, the
// base URL, and the (decrypted) token, so it lives in one place.
const (
	TypeVerifyEmail        = "verify_email"
	TypePasswordReset      = "password_reset"
	TypeEmailChangeConfirm = "email_change_confirm"
	TypeEmailChangeWarning = "email_change_warning"
)

// maxAttempts is the delivery cap: attempt 1 is immediate, failures 1-4 back
// off, and failure 5 dead-letters the row.
const maxAttempts = 5

// backoff is indexed by the attempt count already recorded on the row after a
// failure: a row with attempts=1 next tries in 1m, attempts=2 in 5m, and so on.
var backoff = []time.Duration{
	time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
}

// Cipher is the subset of *crypto.Cipher this package needs. A nil cipher is a
// passthrough, used only by tests; production always injects the master cipher.
type Cipher interface {
	Enabled() bool
	Locked() bool
	Encrypt(aad []byte, plaintext string) (string, error)
	Decrypt(aad []byte, stored string) (string, error)
}

// QueuedMessage is a message to enqueue. Token is the raw one-time secret and is
// the only field encrypted at rest; it may be empty (e.g. the warning message).
type QueuedMessage struct {
	UserID    string
	Type      string
	Recipient string
	Params    map[string]string
	Token     string
}

// MailQueue is the narrow interface identity flows depend on. Enqueue must be
// called with the transaction that commits the token; Nudge is called only
// after that transaction commits.
type MailQueue interface {
	Enqueue(ctx context.Context, tx *sql.Tx, msg QueuedMessage) error
	Nudge()
}

// DeadLetter describes a row that exhausted its retries. The injected callback
// records the platform audit event so this package needs no audit dependency.
type DeadLetter struct {
	ID        string
	Type      string
	Recipient string
	Attempts  int
	LastError string
}

// DeadLetterFunc reports a dead-lettered row. It must not return an error: a
// failed audit write is logged by the caller's implementation.
type DeadLetterFunc func(ctx context.Context, row DeadLetter)

// Outbox owns the mail_outbox table and its delivery worker.
type Outbox struct {
	db         *sql.DB
	mailer     *mailer.Manager
	baseURL    string
	cipher     Cipher
	logger     *slog.Logger
	onDead     DeadLetterFunc
	nudge      chan struct{}
	interval   time.Duration
	batchLimit int
}

// New constructs an Outbox. baseURL is the public origin used to build links;
// onDead may be nil.
func New(db *sql.DB, m *mailer.Manager, baseURL string, c Cipher, logger *slog.Logger, onDead DeadLetterFunc) *Outbox {
	return &Outbox{
		db: db, mailer: m, baseURL: baseURL, cipher: c, logger: logger, onDead: onDead,
		nudge: make(chan struct{}, 1), interval: 30 * time.Second, batchLimit: 50,
	}
}

// SetInterval overrides the worker tick. It is intended for tests.
func (o *Outbox) SetInterval(d time.Duration) {
	if d > 0 {
		o.interval = d
	}
}

func aad(rowID string) []byte {
	return crypto.AAD("tiller", "mailoutbox", rowID, "token")
}

func (o *Outbox) seal(rowID, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	if o.cipher == nil {
		return token, nil
	}
	if o.cipher.Locked() {
		return "", crypto.ErrNoKey
	}
	if !o.cipher.Enabled() {
		return token, nil
	}
	return o.cipher.Encrypt(aad(rowID), token)
}

func (o *Outbox) open(rowID, stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if o.cipher == nil {
		return stored, nil
	}
	if o.cipher.Locked() {
		return "", crypto.ErrLocked
	}
	if !o.cipher.Enabled() {
		return stored, nil
	}
	return o.cipher.Decrypt(aad(rowID), stored)
}

// Enqueue inserts the message inside the caller's transaction. The raw token is
// encrypted before it reaches the database.
func (o *Outbox) Enqueue(ctx context.Context, tx *sql.Tx, msg QueuedMessage) error {
	if msg.Type == "" || msg.Recipient == "" {
		return errors.New("mailoutbox: type and recipient are required")
	}
	rowID, err := id.New()
	if err != nil {
		return err
	}
	sealed, err := o.seal(rowID, msg.Token)
	if err != nil {
		return err
	}
	params := "{}"
	if len(msg.Params) > 0 {
		encoded, merr := json.Marshal(msg.Params)
		if merr != nil {
			return merr
		}
		params = string(encoded)
	}
	now := formatTime(time.Now())
	var userID any
	if msg.UserID != "" {
		userID = msg.UserID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mail_outbox(id,user_id,type,recipient,params,token_ciphertext,attempts,next_attempt_at,created_at) VALUES(?,?,?,?,?,?,0,?,?)`,
		rowID, userID, msg.Type, msg.Recipient, params, nullableText(sealed), now, now)
	return err
}

// Nudge wakes the worker without blocking. It is called after a successful
// commit; a lost nudge is harmless because the ticker retries.
func (o *Outbox) Nudge() {
	select {
	case o.nudge <- struct{}{}:
	default:
	}
}

// Start runs the worker until ctx is cancelled. It performs one pass
// immediately so rows enqueued while the process was down are retried.
func (o *Outbox) Start(ctx context.Context) {
	if o == nil || o.db == nil {
		return
	}
	go func() {
		o.processDue(ctx)
		ticker := time.NewTicker(o.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.processDue(ctx)
			case <-o.nudge:
				o.processDue(ctx)
			}
		}
	}()
}

type dueRow struct {
	id        string
	msgType   string
	recipient string
	params    string
	token     string
	attempts  int
}

// processDue sends the due rows in order. A missing mail provider stops the
// pass immediately rather than generating the same failure for every row.
func (o *Outbox) processDue(ctx context.Context) {
	if o.mailer == nil {
		return
	}
	now := formatTime(time.Now())
	rows, err := o.db.QueryContext(ctx, `SELECT id,type,recipient,params,coalesce(token_ciphertext,''),attempts FROM mail_outbox WHERE sent_at IS NULL AND dead_at IS NULL AND next_attempt_at<=? ORDER BY next_attempt_at,created_at LIMIT ?`, now, o.batchLimit)
	if err != nil {
		o.warn("mail outbox query failed", err)
		return
	}
	var due []dueRow
	for rows.Next() {
		var row dueRow
		if err := rows.Scan(&row.id, &row.msgType, &row.recipient, &row.params, &row.token, &row.attempts); err != nil {
			rows.Close()
			o.warn("mail outbox scan failed", err)
			return
		}
		due = append(due, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		o.warn("mail outbox iteration failed", err)
		return
	}
	rows.Close()

	for _, row := range due {
		if ctx.Err() != nil {
			return
		}
		message, err := o.buildMessage(row)
		if err != nil {
			if errors.Is(err, crypto.ErrLocked) || errors.Is(err, crypto.ErrNoKey) {
				// Locked master key: defer without consuming an attempt.
				return
			}
			o.fail(ctx, row, err)
			continue
		}
		if err := o.mailer.Send(ctx, message); err != nil {
			if errors.Is(err, mailer.ErrNotConfigured) {
				// Provider unavailable: defer the whole pass, consume no attempt.
				return
			}
			o.fail(ctx, row, err)
			continue
		}
		if _, err := o.db.ExecContext(ctx, `UPDATE mail_outbox SET sent_at=?,token_ciphertext=NULL,last_error=NULL WHERE id=?`, formatTime(time.Now()), row.id); err != nil {
			o.warn("mail outbox completion write failed", err)
		}
	}
}

func (o *Outbox) buildMessage(row dueRow) (mailer.Message, error) {
	token, err := o.open(row.id, row.token)
	if err != nil {
		return mailer.Message{}, err
	}
	params := map[string]string{}
	if row.params != "" {
		_ = json.Unmarshal([]byte(row.params), &params)
	}
	switch row.msgType {
	case TypeVerifyEmail:
		if token == "" {
			return mailer.Message{}, errors.New("mailoutbox: missing verification token")
		}
		return mailer.Message{To: row.recipient, Subject: "Verify your Tiller account",
			Text: fmt.Sprintf("Verify your Tiller account:\n\n%s/verify-email?token=%s\n\nThis link expires in 24 hours.", o.baseURL, token)}, nil
	case TypePasswordReset:
		if token == "" {
			return mailer.Message{}, errors.New("mailoutbox: missing reset token")
		}
		return mailer.Message{To: row.recipient, Subject: "Reset your Tiller password",
			Text: fmt.Sprintf("Reset your Tiller password:\n\n%s/reset-password?token=%s\n\nThis link expires in 1 hour.", o.baseURL, token)}, nil
	case TypeEmailChangeConfirm:
		if token == "" {
			return mailer.Message{}, errors.New("mailoutbox: missing email-change token")
		}
		return mailer.Message{To: row.recipient, Subject: "Confirm your new Tiller email address",
			Text: fmt.Sprintf("Confirm your new Tiller email address:\n\n%s/confirm-email-change?token=%s\n\nThis link expires in 24 hours. If you did not request this change, you can ignore this message; your current address keeps working until you confirm.", o.baseURL, token)}, nil
	case TypeEmailChangeWarning:
		return mailer.Message{To: row.recipient, Subject: "Your Tiller email address is being changed",
			Text: fmt.Sprintf("A request was made to change the email address on your Tiller account to %s.\n\nIf this was not you, reset your password immediately to cancel the change: %s/forgot-password\n\nThe change does not take effect until the new address is confirmed.", params["new_email"], o.baseURL)}, nil
	default:
		return mailer.Message{}, errors.New("mailoutbox: unknown message type")
	}
}

// fail records a delivery failure. The fifth failure dead-letters the row and
// invokes the injected audit callback.
func (o *Outbox) fail(ctx context.Context, row dueRow, sendErr error) {
	attempts := row.attempts + 1
	lastError := classify(sendErr)
	if attempts >= maxAttempts {
		if _, err := o.db.ExecContext(ctx, `UPDATE mail_outbox SET attempts=?,dead_at=?,last_error=?,token_ciphertext=NULL WHERE id=?`, attempts, formatTime(time.Now()), lastError, row.id); err != nil {
			o.warn("mail outbox dead-letter write failed", err)
		}
		if o.onDead != nil {
			o.onDead(ctx, DeadLetter{ID: row.id, Type: row.msgType, Recipient: row.recipient, Attempts: attempts, LastError: lastError})
		}
		return
	}
	delay := backoff[min(attempts, len(backoff))-1]
	next := formatTime(time.Now().Add(delay))
	if _, err := o.db.ExecContext(ctx, `UPDATE mail_outbox SET attempts=?,next_attempt_at=?,last_error=? WHERE id=?`, attempts, next, lastError, row.id); err != nil {
		o.warn("mail outbox retry write failed", err)
	}
}

// Cleanup removes sent and dead rows older than retention. It is called by the
// scheduled maintenance pass, not the worker.
func (o *Outbox) Cleanup(ctx context.Context, retention time.Duration) (int64, error) {
	if o == nil || o.db == nil {
		return 0, nil
	}
	cutoff := formatTime(time.Now().Add(-retention))
	result, err := o.db.ExecContext(ctx, `DELETE FROM mail_outbox WHERE (sent_at IS NOT NULL AND sent_at < ?) OR (dead_at IS NOT NULL AND dead_at < ?)`, cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DueCounts reports queued and dead rows for the operator dashboard. Dead is
// scoped to the recent window so an all-time total cannot masquerade as an
// active incident.
func (o *Outbox) DueCounts(ctx context.Context, deadWindow time.Duration) (queued int, dead int, err error) {
	if o == nil || o.db == nil {
		return 0, 0, nil
	}
	if err := o.db.QueryRowContext(ctx, `SELECT count(*) FROM mail_outbox WHERE sent_at IS NULL AND dead_at IS NULL`).Scan(&queued); err != nil {
		return 0, 0, err
	}
	cutoff := formatTime(time.Now().Add(-deadWindow))
	if err := o.db.QueryRowContext(ctx, `SELECT count(*) FROM mail_outbox WHERE dead_at IS NOT NULL AND dead_at >= ?`, cutoff).Scan(&dead); err != nil {
		return 0, 0, err
	}
	return queued, dead, nil
}

func classify(err error) string {
	switch {
	case errors.Is(err, mailer.ErrNotConfigured):
		return "not_configured"
	case errors.Is(err, mailer.ErrInvalidConfig):
		return "invalid_config"
	default:
		return fmt.Sprintf("%T", err)
	}
}

func (o *Outbox) warn(msg string, err error) {
	if o.logger == nil {
		return
	}
	o.logger.Warn(msg, "error_class", fmt.Sprintf("%T", err))
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
