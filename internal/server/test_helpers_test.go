package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
	"github.com/tiller-router/tiller-router/internal/store"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// putOAuthToken seeds an OAuth token in the local account for tests.
func putOAuthToken(t *testing.T, db *database.DB, record oauth.TokenRecord) {
	t.Helper()
	if err := store.New(db.SQL).For(database.LocalAccountID).PutOAuthToken(context.Background(), oauth.TokenToStore(record)); err != nil {
		t.Fatal(err)
	}
}

// getOAuthToken reads a local-account OAuth token for tests.
func getOAuthToken(t *testing.T, db *database.DB, providerID string) oauth.TokenRecord {
	t.Helper()
	row, err := store.New(db.SQL).For(database.LocalAccountID).GetOAuthToken(context.Background(), providerID)
	if err != nil {
		t.Fatal(err)
	}
	return oauth.TokenFromStore(row)
}

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
	// Disable the usage-aggregate cache so tests that write request_logs and
	// then read /api/admin/usage observe their own writes deterministically.
	// Production keeps the TTL set by New; dedicated tests opt back in.
	app.usageCacheTTL = 0
	return app
}
