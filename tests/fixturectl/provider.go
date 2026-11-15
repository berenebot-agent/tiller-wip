package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/tiller-router/tiller-router/internal/database"
)

// providerOutput is the machine-readable JSON emitted by the provider
// subcommand.
type providerOutput struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	BaseURL   string   `json:"base_url"`
	ModelIDs  []string `json:"model_ids"`
	ModelPKs  []string `json:"model_pks"`
	Namespace string   `json:"namespace"`
}

// runProvider seeds a provider plus its discovered models directly into the
// router database.
//
// It exists for the same reason runClient does: creating a provider over HTTP
// (POST /api/admin/providers) runs live model discovery against the upstream,
// which costs a network round-trip and -- more importantly for browser tests --
// spends the account's console request budget. Tests that merely need a
// provider to exist (permission dialogs, routing pickers, mobile cards) should
// not pay for discovery they never assert on.
//
// The seeded provider is indistinguishable from a discovered one at rest:
// enabled, protocols=chat, last_refresh_at set to now (so the console does not
// treat it as awaiting a first refresh) and one provider_models row per
// --model. Origin is left at 'discovered' because that is what these providers
// represent; --origin manual flips it for tests that need a hand-added model.
//
// Everything is written in one transaction and the invocation is idempotent:
// re-running with the same name deletes the prior provider, its models, its
// namespace entry and any permissions pointing at those models, then
// re-inserts. That mirrors runClient and keeps fixed-name fixtures safe across
// re-runs.
func runProvider(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("provider", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to the router SQLite database (required)")
	name := fs.String("name", "", "provider name, lowercase slug (required, must be unique)")
	provType := fs.String("type", "generic-openai", "provider type")
	baseURL := fs.String("base-url", "http://127.0.0.1:1/v1", "upstream base URL")
	models := fs.String("models", "mock-model", "comma-separated upstream model ids to seed")
	origin := fs.String("origin", "discovered", "provider_models.origin: discovered or manual")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" || *name == "" {
		return errors.New("--db and --name are required")
	}
	if *origin != "discovered" && *origin != "manual" {
		return fmt.Errorf("invalid --origin %q (expected discovered or manual)", *origin)
	}

	var modelIDs []string
	for _, raw := range strings.Split(*models, ",") {
		if id := strings.TrimSpace(raw); id != "" {
			modelIDs = append(modelIDs, id)
		}
	}
	if len(modelIDs) == 0 {
		return errors.New("--models must name at least one model")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("open database %s: %w", *dbPath, err)
	}
	defer db.Close()

	id := "browser-" + *name
	now := database.Now()

	tx, err := db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Idempotency: clear anything a previous run left behind. Order matters --
	// the namespace row is referenced by providers via a composite FK, so
	// children go first. Permissions pointing at these models are cleared too,
	// otherwise the provider delete would hit the FK from
	// client_model_permissions.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM client_model_permissions WHERE model_kind='real' AND model_id IN (SELECT id FROM provider_models WHERE provider_id=?)`,
		id); err != nil {
		return fmt.Errorf("clear model permissions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM provider_models WHERE provider_id=?`, id); err != nil {
		return fmt.Errorf("clear prior models: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM providers WHERE id=? OR name=?`, id, *name); err != nil {
		return fmt.Errorf("clear prior provider: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM namespaces WHERE name=? AND kind='real'`, *name); err != nil {
		return fmt.Errorf("clear prior namespace: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO namespaces(name,kind,entity_id) VALUES(?,?,?)`,
		*name, "real", id); err != nil {
		return fmt.Errorf("insert namespace: %w", err)
	}

	// last_refresh_at/next_refresh_at are set so the console treats the provider
	// as freshly discovered rather than awaiting its first refresh (which would
	// render a "never refreshed" state and sometimes trigger a follow-up fetch --
	// exactly the cost this fixture exists to avoid).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO providers(id,name,type,base_url,enabled,protocols,last_refresh_at,next_refresh_at,created_at,updated_at)
		 VALUES(?,?,?,?,1,'["chat"]',?,?,?,?)`,
		id, *name, *provType, *baseURL, now, now, now, now); err != nil {
		return fmt.Errorf("insert provider: %w", err)
	}

	modelPKs := make([]string, 0, len(modelIDs))
	for _, upstream := range modelIDs {
		pk := "browser-" + *name + "/" + upstream
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO provider_models(id,provider_id,upstream_model_id,display_name,available,origin,first_seen_at,last_seen_at,created_at,updated_at)
			 VALUES(?,?,?,?,1,?,?,?,?,?)`,
			pk, id, upstream, upstream, *origin, now, now, now, now); err != nil {
			return fmt.Errorf("insert model %q: %w", upstream, err)
		}
		modelPKs = append(modelPKs, pk)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(providerOutput{
		ID:        id,
		Name:      *name,
		Type:      *provType,
		BaseURL:   *baseURL,
		ModelIDs:  modelIDs,
		ModelPKs:  modelPKs,
		Namespace: *name,
	})
}
