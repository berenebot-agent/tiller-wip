package server

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// testLiveTimings are the short debounce/idle/session-check intervals used by
// test servers so live/SSE tests do not wait on production-scale real time.
// Production defaults (live.go) are unchanged.
var testLiveTimings = liveTimings{debounce: 10 * time.Millisecond, idle: 10 * time.Millisecond, sessionCheck: 10 * time.Millisecond}

// newTestServer constructs a Server using the fast test hasher and short live
// timings. Most server tests verify routing/permissions/activity/notification
// semantics, which do not require the 64 MiB Argon2id cost; the focused
// production-hasher tests in internal/auth cover the real KDF path.
func newTestServer(t *testing.T, cfg config.Config, db *database.DB) *Server {
	t.Helper()
	app, err := New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	app.liveHub.timings = testLiveTimings
	return app
}
