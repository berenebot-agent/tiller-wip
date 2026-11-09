package server

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// syncBuffer collects log output written from the scheduler goroutine without
// racing the assertions.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestMaintenanceSchedulerLogsBackupCost pins the observable contract added for
// sizing the backup pass: a successful snapshot logs at Info with its duration
// and size. Without this line a finished backup was invisible in the logs, so
// "how long does it take" could only be guessed from file timestamps.
func TestMaintenanceSchedulerLogsBackupCost(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var logs syncBuffer
	cfg := config.Config{
		TillerUser:         "admin",
		TillerUserPassword: "maintenance-admin-fixture",
		DataDir:            t.TempDir(),
		ListenAddr:         ":8080",
		BackupInterval:     20 * time.Millisecond,
		BackupRetention:    time.Hour,
	}
	app, err := New(cfg, db, slog.New(slog.NewTextHandler(io.Writer(&logs), nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.startMaintenanceScheduler(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(logs.String(), "database backup completed") {
		if time.Now().After(deadline) {
			t.Fatalf("scheduler never logged a completed backup; logs:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	line := ""
	for _, candidate := range strings.Split(logs.String(), "\n") {
		if strings.Contains(candidate, "database backup completed") {
			line = candidate
			break
		}
	}
	for _, want := range []string{"db=core", "duration_ms=", "size_bytes="} {
		if !strings.Contains(line, want) {
			t.Fatalf("backup log line missing %q: %s", want, line)
		}
	}
}
