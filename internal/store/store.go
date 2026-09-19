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
	"database/sql"
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
	return &Scope{db: s.db, accountID: accountID}
}

// Scope is an account-scoped view of the tenant tables. Its methods can only
// touch rows belonging to AccountID.
type Scope struct {
	db        *sql.DB
	accountID string
}

// AccountID returns the account this scope is bound to.
func (s *Scope) AccountID() string { return s.accountID }

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
