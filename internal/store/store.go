// Package store is the single boundary for tenant-table SQL.
//
// Every tenant-owned table is account-scoped. A *Store wraps a *sql.DB;
// Store.For(accountID) returns a *Scope whose methods always read and write
// rows for exactly one account. Handlers must obtain their scope from an
// authenticated principal (an admin session owning the account, or a client
// API key bound to the account) — never from request input.
//
// The account classification of every table is enforced by
// database.ClassifiedTables and the guard test in internal/database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"time"
)

// errNoActivityStore is returned when an Activity operation is attempted on a
// Store that was not configured with an Activity database handle. Core-only
// callers (auth, discovery, settings) construct Store without one.
var errNoActivityStore = errors.New("store: activity store is not configured")

// activeTenantTxs counts currently open tenant transactions across all scopes.
// It exists so a runtime guard test can prove no tenant transaction is held
// across provider network I/O or client streaming (AGENTS.md tenancy invariants
// and sass_tech.md §8.4). It is a diagnostic counter, not a control.
var activeTenantTxs atomic.Int64

// ActiveTenantTransactions reports the number of tenant transactions currently
// open. Tests use it to assert the invariant; production code never branches on
// it.
func ActiveTenantTransactions() int64 { return activeTenantTxs.Load() }

// Store owns the database handles and hands out account-scoped handles.
type Store struct {
	db       *sql.DB
	audit    *sql.DB
	activity *sql.DB
	cipher   SecretCipher
}

// Option configures a Store.
type Option func(*Store)

// WithActivityDB injects the separate Activity database handle. A Store
// without it can still serve control-plane queries but any Activity method
// returns errNoActivityStore.
func WithActivityDB(db *sql.DB) Option {
	return func(s *Store) { s.activity = db }
}

// WithCipher injects the recoverable-secret cipher used to encrypt and decrypt
// provider credentials, OAuth tokens, and secret settings. Without it the
// store passes secrets through in plaintext, which is only appropriate for
// tests and hash-only paths; production always injects one.
func WithCipher(c SecretCipher) Option {
	return func(s *Store) {
		if c != nil {
			s.cipher = c
		}
	}
}

// WithAuditDB enables the separate central audit database. Audit methods are
// no-ops only when no audit handle is supplied (for tests that exercise tenant
// resources without audit persistence).
func WithAuditDB(db *sql.DB) Option {
	return func(s *Store) { s.audit = db }
}

func New(db *sql.DB, opts ...Option) *Store {
	s := &Store{db: db, cipher: disabledCipher{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// DB exposes the underlying pool for platform-global tables only
// (schema_migrations, accounts, admin_sessions, platform_settings). Tenant
// tables must not be queried through it.
func (s *Store) DB() *sql.DB { return s.db }

// AuditDB exposes the separate central audit handle for platform-only jobs
// such as retention pruning. Tenant audit reads/writes use Scope methods.
func (s *Store) AuditDB() *sql.DB { return s.audit }

// DeleteAccountActivity removes the account's Activity rows. The
// request_attempts foreign key cascades, so both tables are cleared.
func (s *Store) DeleteAccountActivity(accountID string) error {
	if s.activity == nil {
		return nil
	}
	_, err := s.activity.ExecContext(context.Background(), `DELETE FROM request_logs WHERE account_id=?`, accountID)
	return err
}

// For returns a handle scoped to one account. accountID must come from a
// verified principal.
func (s *Store) For(accountID string) *Scope {
	return &Scope{db: s.db, q: s.db, audit: s.audit, accountID: accountID, activity: s.activity, cipher: s.cipher}
}

// querier is satisfied by both *sql.DB and *sql.Tx so a Scope works inside and
// outside a transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// errNestedTx guards against opening a transaction on an already-transactional
// scope, which would silently escape the enclosing transaction.
var errNestedTx = errors.New("store: cannot begin transaction within a transaction")

// Scope is an account-scoped view of the tenant tables. Its methods can only
// touch rows belonging to AccountID.
type Scope struct {
	db        *sql.DB
	q         querier
	audit     *sql.DB
	accountID string
	activity  *sql.DB
	cipher    SecretCipher
}

// AccountID returns the account this scope is bound to.
func (s *Scope) AccountID() string { return s.accountID }

// RunTx runs fn inside a database transaction bound to the same account. The
// scope passed to fn reads and writes within the transaction; the outer scope
// is never used for tenant SQL while the transaction is open. A nil error
// commits, any other error rolls back. RunTx must not be called on a scope that
// is already inside a transaction.
func (s *Scope) RunTx(ctx context.Context, opts *sql.TxOptions, fn func(*Scope) error) error {
	if s.db == nil {
		return errNestedTx
	}
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	activeTenantTxs.Add(1)
	defer activeTenantTxs.Add(-1)
	child := &Scope{db: nil, q: tx, audit: s.audit, accountID: s.accountID, activity: s.activity, cipher: s.cipher}
	if err := fn(child); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// withActivity runs fn against the shared Activity database. Every caller's
// SQL is account-scoped through the Scope's accountID.
func (s *Scope) withActivity(ctx context.Context, fn func(q querier) error) error {
	if s.activity == nil {
		return errNoActivityStore
	}
	return fn(s.activity)
}

// runActivityTx runs fn inside a transaction on the Activity database.
// request_logs and request_attempts are written in one transaction, so a single
// request's log and its attempts commit together.
func (s *Scope) runActivityTx(ctx context.Context, fn func(q querier) error) error {
	if s.activity == nil {
		return errNoActivityStore
	}
	tx, err := s.activity.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
