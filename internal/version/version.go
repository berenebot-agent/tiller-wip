// Package version contains build metadata injected by the release build.
package version

// These defaults make local development and unversioned images explicit. The
// Dockerfile and release workflow override them with -ldflags -X values.
var (
	Version = "dev"
	Commit  = "unknown"
)
