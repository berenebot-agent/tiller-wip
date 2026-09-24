package mailoutbox

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/crypto"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/mailer"
)

// noopMailer is a mailer.Manager with no provider, so Send returns
// ErrNotConfigured. It exercises the deferral path without a network.
func newManagerWith(t *testing.T) *mailer.Manager {
	t.Helper()
	m, err := mailer.NewManager(mailer.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func testCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, crypto.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c, err := crypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newTestOutbox(t *testing.T) (*Outbox, *sql.DB) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db.SQL, newManagerWith(t), "https://app.example.com", testCipher(t), nil, nil), db.SQL
}

func enqueueOnce(t *testing.T, o *Outbox, msg QueuedMessage) string {
	t.Helper()
	ctx := context.Background()
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Enqueue(ctx, tx, msg); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := o.db.QueryRowContext(ctx, `SELECT id FROM mail_outbox ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEnqueueEncryptsTokenAtRest(t *testing.T) {
	o, _ := newTestOutbox(t)
	id := enqueueOnce(t, o, QueuedMessage{Type: TypeVerifyEmail, Recipient: "a@example.com", Token: "raw-secret-token"})
	var stored string
	if err := o.db.QueryRow(`SELECT token_ciphertext FROM mail_outbox WHERE id=?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "raw-secret-token" || !crypto.Encrypted(stored) {
		t.Fatalf("token at rest = %q, want enc:v1 ciphertext", stored)
	}
	plain, err := o.open(id, stored)
	if err != nil || plain != "raw-secret-token" {
		t.Fatalf("decrypted token = %q, err = %v", plain, err)
	}
}

// TestLockedCipherNeverReturnsCiphertextAsPlaintext covers the bug where a
// locked cipher reported Enabled()==false and open() handed the stored enc:v1:
// value back as if it were plaintext.
func TestLockedCipherNeverReturnsCiphertextAsPlaintext(t *testing.T) {
	o, _ := newTestOutbox(t)
	id := enqueueOnce(t, o, QueuedMessage{Type: TypeVerifyEmail, Recipient: "a@example.com", Token: "raw-secret-token"})
	var stored string
	if err := o.db.QueryRow(`SELECT token_ciphertext FROM mail_outbox WHERE id=?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !crypto.Encrypted(stored) {
		t.Fatalf("token at rest = %q, want enc:v1 ciphertext", stored)
	}
	o.cipher = crypto.Locked()
	if plain, err := o.open(id, stored); !errors.Is(err, crypto.ErrLocked) {
		t.Fatalf("open on locked cipher = (%q, %v), want crypto.ErrLocked", plain, err)
	}
}

func TestNotConfiguredDefersWithoutConsumingAttempt(t *testing.T) {
	o, _ := newTestOutbox(t)
	enqueueOnce(t, o, QueuedMessage{Type: TypeVerifyEmail, Recipient: "a@example.com", Token: "tok"})
	o.processDue(context.Background())
	var attempts int
	if err := o.db.QueryRow(`SELECT attempts FROM mail_outbox`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("attempts after ErrNotConfigured = %d, want 0", attempts)
	}
	var dead sql.NullString
	if err := o.db.QueryRow(`SELECT dead_at FROM mail_outbox`).Scan(&dead); err != nil {
		t.Fatal(err)
	}
	if dead.Valid {
		t.Fatal("row was dead-lettered on an unconfigured mailer")
	}
}

// failingOutbox exercises the retry/dead-letter arithmetic directly: it uses a
// real manager that always returns a delivery error.
type alwaysFailMailer struct{}

func (alwaysFailMailer) Send(context.Context, mailer.Message) error {
	return errors.New("boom")
}

func TestRetryBackoffAndDeadLetter(t *testing.T) {
	o, _ := newTestOutbox(t)
	o.mailer = nil // force processDue to use our injected sender below
	enqueueOnce(t, o, QueuedMessage{Type: TypeVerifyEmail, Recipient: "a@example.com", Token: "tok"})
	// Drive fail() directly to assert the exact attempt arithmetic.
	var row dueRow
	if err := o.db.QueryRow(`SELECT id,type,recipient,params,coalesce(token_ciphertext,''),attempts FROM mail_outbox`).Scan(&row.id, &row.msgType, &row.recipient, &row.params, &row.token, &row.attempts); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		o.fail(context.Background(), row, errors.New("boom"))
		row.attempts = i
		var attempts int
		var next string
		var dead sql.NullString
		if err := o.db.QueryRow(`SELECT attempts,next_attempt_at,dead_at FROM mail_outbox WHERE id=?`, row.id).Scan(&attempts, &next, &dead); err != nil {
			t.Fatal(err)
		}
		if attempts != i {
			t.Fatalf("attempt %d recorded as %d", i, attempts)
		}
		if dead.Valid {
			t.Fatalf("dead-lettered early at attempt %d", i)
		}
		deadline, err := time.Parse(time.RFC3339Nano, next)
		if err != nil {
			t.Fatal(err)
		}
		wantDelay := backoff[i-1]
		if got := time.Until(deadline); got < wantDelay-time.Minute || got > wantDelay+time.Minute {
			t.Fatalf("attempt %d next delay = %s, want ~%s", i, got, wantDelay)
		}
	}
	// Fifth failure dead-letters.
	o.fail(context.Background(), row, errors.New("boom"))
	var dead sql.NullString
	var attempts int
	var token sql.NullString
	if err := o.db.QueryRow(`SELECT attempts,dead_at,token_ciphertext FROM mail_outbox WHERE id=?`, row.id).Scan(&attempts, &dead, &token); err != nil {
		t.Fatal(err)
	}
	if attempts != maxAttempts || !dead.Valid {
		t.Fatalf("after 5th failure attempts=%d dead=%v", attempts, dead.Valid)
	}
	if token.Valid {
		t.Fatal("dead-lettered row retained its token")
	}
}

func TestDueWorkIndexOnlyCoversLiveRows(t *testing.T) {
	o, _ := newTestOutbox(t)
	enqueueOnce(t, o, QueuedMessage{Type: TypeVerifyEmail, Recipient: "a@example.com", Token: "tok"})
	var name string
	if err := o.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='mail_outbox_due'`).Scan(&name); err != nil {
		t.Fatalf("due-work partial index missing: %v", err)
	}
}

func TestCleanupRemovesSentRows(t *testing.T) {
	o, _ := newTestOutbox(t)
	id := enqueueOnce(t, o, QueuedMessage{Type: TypeVerifyEmail, Recipient: "a@example.com", Token: "tok"})
	if _, err := o.db.Exec(`UPDATE mail_outbox SET sent_at=?,token_ciphertext=NULL WHERE id=?`, formatTime(time.Now().Add(-48*time.Hour)), id); err != nil {
		t.Fatal(err)
	}
	removed, err := o.Cleanup(context.Background(), 24*time.Hour)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
}

func TestDueCounts(t *testing.T) {
	o, _ := newTestOutbox(t)
	enqueueOnce(t, o, QueuedMessage{Type: TypeVerifyEmail, Recipient: "a@example.com", Token: "tok"})
	queued, dead, err := o.DueCounts(context.Background(), 24*time.Hour)
	if err != nil || queued != 1 || dead != 0 {
		t.Fatalf("counts = %d/%d err=%v", queued, dead, err)
	}
}

func TestSentCountAndRecent(t *testing.T) {
	o, _ := newTestOutbox(t)
	sentID := enqueueOnce(t, o, QueuedMessage{Type: TypeVerifyEmail, Recipient: "sent@example.com", Token: "tok"})
	enqueueOnce(t, o, QueuedMessage{Type: TypePasswordReset, Recipient: "queued@example.com", Token: "tok"})
	deadID := enqueueOnce(t, o, QueuedMessage{Type: TypeVerifyEmail, Recipient: "dead@example.com", Token: "tok"})
	if _, err := o.db.Exec(`UPDATE mail_outbox SET sent_at=?,token_ciphertext=NULL WHERE id=?`, formatTime(time.Now()), sentID); err != nil {
		t.Fatal(err)
	}
	if _, err := o.db.Exec(`UPDATE mail_outbox SET dead_at=?,token_ciphertext=NULL WHERE id=?`, formatTime(time.Now()), deadID); err != nil {
		t.Fatal(err)
	}
	sent, err := o.SentCount(context.Background(), 24*time.Hour)
	if err != nil || sent != 1 {
		t.Fatalf("sent = %d err=%v", sent, err)
	}
	log, err := o.Recent(context.Background(), 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 3 {
		t.Fatalf("log len = %d, want 3", len(log))
	}
	statuses := map[string]string{}
	for _, e := range log {
		statuses[e.Recipient] = e.Status
	}
	if statuses["sent@example.com"] != "sent" || statuses["queued@example.com"] != "queued" || statuses["dead@example.com"] != "dead" {
		t.Fatalf("statuses = %v", statuses)
	}
}
