package server

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/providers"
)

// opencodeFreeProviderName is the canonical instance name for the keyless
// OpenCode Free provider. It lives in the shared provider/virtual namespace
// so it must remain unique. The onboarding flow and the manual Add Provider
// dialog both use this name, making the ensure path idempotent.
const opencodeFreeProviderName = "opencode-free"
const opencodeFreeProviderType = "opencode-free"

// ensureOpenCodeFreeProvider returns the provider ID for the keyless
// OpenCode Free instance, creating it if it does not yet exist. Creation is
// idempotent and safe under concurrent callers thanks to the UNIQUE
// constraint on namespaces.name. After insertion the caller is expected to
// refresh the catalogue (Manager.Refresh) or rely on the scheduler; this
// helper does NOT block on network I/O so it can be used from onboarding
// without holding a DB transaction across an HTTP call.
//
// It is intentionally unexported and has no HTTP handler today — the manual
// Add Provider path (POST /api/admin/providers with type opencode-free)
// already works via the generic createProvider flow. The helper exists so
// the first-run wizard (docs/roadmap_first_run.md Page 2) can call it without
// duplicating the namespace/provider insertion logic.
func (s *Server) ensureOpenCodeFreeProvider(ctx context.Context) (string, error) {
	var existing string
	err := s.db.SQL.QueryRowContext(ctx, `SELECT id FROM providers WHERE name=?`, opencodeFreeProviderName).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	descriptor, ok := providers.Lookup(opencodeFreeProviderType)
	if !ok {
		return "", sql.ErrNoRows
	}
	baseURL := strings.TrimRight(descriptor.DefaultBaseURL, "/")
	protocols := descriptor.Protocols
	providerID, err := id.New()
	if err != nil {
		return "", err
	}
	now := database.Now()
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO namespaces(name,kind,entity_id) VALUES(?,'real',?)`, opencodeFreeProviderName, providerID); err != nil {
		if database.IsConstraint(err) {
			// Lost race — another caller inserted first. Return the winner.
			var winner string
			if qerr := s.db.SQL.QueryRowContext(ctx, `SELECT id FROM providers WHERE name=?`, opencodeFreeProviderName).Scan(&winner); qerr == nil {
				return winner, nil
			}
		}
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO providers(id,name,type,base_url,credential_secret,enabled,protocols,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, providerID, opencodeFreeProviderName, opencodeFreeProviderType, baseURL, nil, 1, providers.EncodeProtocols(protocols), now, now); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at) SELECT id,'real',?,0,? FROM client_keys`, providerID, now); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		if database.IsConstraint(err) {
			var winner string
			if qerr := s.db.SQL.QueryRowContext(ctx, `SELECT id FROM providers WHERE name=?`, opencodeFreeProviderName).Scan(&winner); qerr == nil {
				return winner, nil
			}
		}
		return "", err
	}
	// Best-effort synchronous refresh with a bounded timeout so the caller
	// sees a populated catalogue immediately (mirrors createProvider's 3m
	// refresh). Failure is not fatal — scheduler will retry and the provider
	// remains stored with last_refresh_error.
	refreshCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_ = s.providers.Refresh(refreshCtx, providerID)
	return providerID, nil
}
