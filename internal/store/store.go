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
	"time"
)

// Store owns the database handle and hands out account-scoped handles.
type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// DB exposes the underlying pool for platform-global tables only
// (schema_migrations, accounts, admin_sessions, platform_settings). Tenant
// tables must not be queried through it.
func (s *Store) DB() *sql.DB { return s.db }

// For returns a handle scoped to one account. accountID must come from a
// verified principal.
func (s *Store) For(accountID string) *Scope {
	return &Scope{db: s.db, q: s.db, accountID: accountID}
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
	accountID string
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
	child := &Scope{db: nil, q: tx, accountID: s.accountID}
	if err := fn(child); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
