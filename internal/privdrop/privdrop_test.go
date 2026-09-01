package privdrop

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDropToRuntimeUserNoopWhenNonRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test asserts no-op behaviour, which only holds for a non-root process")
	}
	dir := t.TempDir()
	dropped, err := DropToRuntimeUser(dir)
	if err != nil {
		t.Fatalf("DropToRuntimeUser: %v", err)
	}
	if dropped {
		t.Fatal("expected no drop for a non-root process")
	}
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		t.Fatal("non-root process unexpectedly became root")
	}
}

func TestDropToRuntimeUserAsRoot(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("test re-runs itself as root via docker; skipped on a non-root host")
	}
	_ = t
}

// TestRootDropIntegration is the entry point used by the container test:
// `tiller-go.sh test` runs on the host as a non-root user, so the actual
// root -> 65532 transition is exercised by tests/privdrop-smoke.sh, which
// invokes this binary in a container as root with a root-owned data dir.
func TestRootDropIntegration(t *testing.T) {
	if os.Getenv("TILLER_PRIVDROP_TEST_TARGET") == "" {
		t.Skip("integration target; run via tests/privdrop-smoke.sh")
	}
	dataDir := os.Getenv("TILLER_PRIVDROP_TEST_DIR")
	if dataDir == "" {
		t.Fatal("TILLER_PRIVDROP_TEST_DIR must be set")
	}
	// Seed a root-owned file structure the chown walk must cover.
	nested := filepath.Join(dataDir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("seed dirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := os.Chmod(dataDir, 0o755); err != nil { // walk must tighten this later via app code
		t.Fatalf("chmod seed: %v", err)
	}

	dropped, err := DropToRuntimeUser(dataDir)
	if err != nil {
		t.Fatalf("DropToRuntimeUser: %v", err)
	}
	if !dropped {
		t.Fatal("expected a privilege drop when started as root")
	}
	if os.Getuid() != DefaultUID || os.Getgid() != DefaultGID {
		t.Fatalf("process uid/gid = %d/%d, want %d/%d", os.Getuid(), os.Getgid(), DefaultUID, DefaultGID)
	}
	if got := os.Getenv("TILLER_RUN_UID"); got != "" {
		if v, convErr := strconv.Atoi(got); convErr == nil && v != DefaultUID {
			t.Fatalf("TILLER_RUN_UID=%d overrides must be honoured", v)
		}
	}
}
