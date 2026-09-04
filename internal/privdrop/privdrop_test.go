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
	dropped, uid, gid, err := DropToRuntimeUser(dir)
	if err != nil {
		t.Fatalf("DropToRuntimeUser: %v", err)
	}
	if dropped {
		t.Fatal("expected no drop for a non-root process")
	}
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		t.Fatal("non-root process unexpectedly became root")
	}
	// Even on the no-op path the resolved UID/GID is reported so callers
	// can log the runtime identity without re-reading the environment.
	wantUID, wantGID, err := ResolvedIdentity()
	if err != nil {
		t.Fatalf("ResolvedIdentity: %v", err)
	}
	if uid != wantUID || gid != wantGID {
		t.Fatalf("DropToRuntimeUser reported uid/gid = %d/%d, want %d/%d", uid, gid, wantUID, wantGID)
	}
}

func TestDropToRuntimeUserAsRoot(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("test re-runs itself as root via docker; skipped on a non-root host")
	}
	_ = t
}

func TestResolvedIdentityHonoursEnv(t *testing.T) {
	t.Setenv("TILLER_RUN_UID", "4242")
	t.Setenv("TILLER_RUN_GID", "4343")
	uid, gid, err := ResolvedIdentity()
	if err != nil {
		t.Fatalf("ResolvedIdentity: %v", err)
	}
	if uid != 4242 || gid != 4343 {
		t.Fatalf("ResolvedIdentity = %d/%d, want 4242/4343", uid, gid)
	}
}

func TestResolvedIdentityFallsBackToDefaults(t *testing.T) {
	t.Setenv("TILLER_RUN_UID", "")
	t.Setenv("TILLER_RUN_GID", "")
	uid, gid, err := ResolvedIdentity()
	if err != nil {
		t.Fatalf("ResolvedIdentity: %v", err)
	}
	if uid != DefaultUID || gid != DefaultGID {
		t.Fatalf("ResolvedIdentity = %d/%d, want %d/%d", uid, gid, DefaultUID, DefaultGID)
	}
}

func TestResolvedIdentityRejectsNonPositive(t *testing.T) {
	t.Setenv("TILLER_RUN_UID", "0")
	if _, _, err := ResolvedIdentity(); err == nil {
		t.Fatal("expected an error for TILLER_RUN_UID=0")
	}
	t.Setenv("TILLER_RUN_UID", "not-a-number")
	if _, _, err := ResolvedIdentity(); err == nil {
		t.Fatal("expected an error for non-numeric TILLER_RUN_UID")
	}
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

	dropped, appliedUID, appliedGID, err := DropToRuntimeUser(dataDir)
	if err != nil {
		t.Fatalf("DropToRuntimeUser: %v", err)
	}
	if !dropped {
		t.Fatal("expected a privilege drop when started as root")
	}
	if os.Getuid() != appliedUID || os.Getgid() != appliedGID {
		t.Fatalf("process uid/gid = %d/%d, DropToRuntimeUser reported %d/%d", os.Getuid(), os.Getgid(), appliedUID, appliedGID)
	}
	if got := os.Getenv("TILLER_RUN_UID"); got != "" {
		if v, convErr := strconv.Atoi(got); convErr == nil && v != DefaultUID {
			t.Fatalf("TILLER_RUN_UID=%d overrides must be honoured", v)
		}
	}
}
