package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

// errNoActivityStore is returned when an Activity operation is attempted on a
// Store that was not configured with an Activity directory. Core-only callers
// (auth, discovery, settings) construct Store without one.
var errNoActivityStore = errors.New("store: activity store is not configured")

// defaultMaxOpenActivityFiles bounds how many per-account Activity files are
// held open at once. Idle handles above the cap are closed; the file is
// reopened on next use, so the bound trades a little open cost for a hard file
// descriptor ceiling on a shared host.
const defaultMaxOpenActivityFiles = 128

// activityFiles owns the lazily opened, per-account Activity database handles.
// It is safe for concurrent use. A handle is reference-counted so it is only
// closed while idle.
type activityFiles struct {
	dir     string
	maxOpen int

	mu    sync.Mutex
	files map[string]*activityFile
}

type activityFile struct {
	db       *sql.DB
	refs     int
	lastUsed time.Time
}

func newActivityFiles(dir string) *activityFiles {
	return &activityFiles{dir: dir, maxOpen: defaultMaxOpenActivityFiles, files: map[string]*activityFile{}}
}

// acquire returns the account's Activity database and a release func that must
// be called when the caller is done. The account id is validated before it is
// used as a filename.
func (m *activityFiles) acquire(ctx context.Context, accountID string) (*sql.DB, func(), error) {
	if m == nil || m.dir == "" {
		return nil, nil, errNoActivityStore
	}
	if err := database.ValidateAccountID(accountID); err != nil {
		return nil, nil, err
	}
	m.mu.Lock()
	f := m.files[accountID]
	if f == nil {
		m.evictLocked()
		db, err := database.OpenActivity(ctx, filepath.Join(m.dir, accountID+".db"))
		if err != nil {
			m.mu.Unlock()
			return nil, nil, err
		}
		f = &activityFile{db: db}
		m.files[accountID] = f
	}
	f.refs++
	f.lastUsed = time.Now()
	m.mu.Unlock()
	return f.db, func() { m.release(accountID) }, nil
}

func (m *activityFiles) release(accountID string) {
	m.mu.Lock()
	if f := m.files[accountID]; f != nil {
		f.refs--
		f.lastUsed = time.Now()
	}
	m.mu.Unlock()
}

// evictLocked closes the least-recently-used idle handle when the open-file cap
// has been reached. It is a no-op while every open handle is in use.
func (m *activityFiles) evictLocked() {
	if len(m.files) < m.maxOpen {
		return
	}
	victim := ""
	var oldest time.Time
	for id, f := range m.files {
		if f.refs != 0 {
			continue
		}
		if victim == "" || f.lastUsed.Before(oldest) {
			victim, oldest = id, f.lastUsed
		}
	}
	if victim == "" {
		return
	}
	_ = m.files[victim].db.Close()
	delete(m.files, victim)
}
