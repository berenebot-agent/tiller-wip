// Package dbtest is test-only support: a drop-in for database.Open that skips
// re-running all schema migrations for every test database.
//
// The first call in a process opens a real fresh database (running every
// migration through the production code path), closes it cleanly and keeps the
// resulting files in memory. Later default-option opens of a not-yet-existing
// path restore those files and then run the real database.Open on them, so the
// pragmas, migration ledger check, activity attach and every other Open step
// still execute. The result is marked FreshInstall like a genuinely fresh open.
//
// Opens with options, or of a path that already exists, go straight to
// database.Open untouched. Set TILLER_DBTEST_NO_TEMPLATE=1 to disable the
// template entirely and run every migration per database.
package dbtest

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/tiller-router/tiller-router/internal/database"
)

var (
	once  sync.Once
	files map[string][]byte // base name -> content
)

func build() {
	files = map[string][]byte{}
	dir, err := os.MkdirTemp("", "dbtest-template-")
	if err != nil {
		return
	}
	defer os.RemoveAll(dir)
	db, err := database.Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		files = nil
		return
	}
	_ = db.Close()
	ents, err := os.ReadDir(dir)
	if err != nil {
		files = nil
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			files = nil
			return
		}
		files[e.Name()] = b
	}
	if _, ok := files["router.db"]; !ok {
		files = nil
	}
}

// Open has the same signature and result as database.Open.
func Open(ctx context.Context, path string, opts ...database.OpenOption) (*database.DB, error) {
	if len(opts) > 0 || os.Getenv("TILLER_DBTEST_NO_TEMPLATE") == "1" || filepath.Base(path) != "router.db" {
		return database.Open(ctx, path, opts...)
	}
	if _, err := os.Stat(path); err == nil {
		return database.Open(ctx, path)
	}
	once.Do(build)
	if files == nil {
		return database.Open(ctx, path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return database.Open(ctx, path)
		}
	}
	d, err := database.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	d.FreshInstall = true
	return d, nil
}
