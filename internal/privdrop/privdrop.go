// Package privdrop covers the deployment case where Tiller is started as
// root — for example `user: "0:0"` in Compose so a Docker-created bind-mount
// data directory can be fixed up without host-side chown commands. It hands
// the data directory to the image's non-root runtime user and then drops
// privileges before the database is opened or any request is served.
//
// When the process is already non-root — the normal case for the published
// image (USER 65532) and every hardened deployment — it is a no-op, so the
// read-only / caps-dropped compose never needs setuid or chown capabilities.
package privdrop

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// DefaultUID and DefaultGID match the image's runtime user (USER 65532).
const (
	DefaultUID = 65532
	DefaultGID = 65532
)

// DropToRuntimeUser chowns dataDir (recursively) to the runtime UID/GID and
// switches the running process to that user. It reports whether a drop
// actually happened. Once dropped, privileges cannot be regained.
func DropToRuntimeUser(dataDir string) (bool, error) {
	if os.Getuid() != 0 {
		return false, nil
	}
	uid, err := envID("TILLER_RUN_UID", DefaultUID)
	if err != nil {
		return false, err
	}
	gid, err := envID("TILLER_RUN_GID", DefaultGID)
	if err != nil {
		return false, err
	}
	if err := chownRecursive(dataDir, uid, gid); err != nil {
		return false, fmt.Errorf("chown data directory: %w", err)
	}
	// Supplementary groups, then group, then user: each call permanently
	// sheds the privileges the next one depends on, so the order matters.
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return false, fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return false, fmt.Errorf("setgid %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return false, fmt.Errorf("setuid %d: %w", uid, err)
	}
	return true, nil
}

func envID(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, raw)
	}
	return v, nil
}

func chownRecursive(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}