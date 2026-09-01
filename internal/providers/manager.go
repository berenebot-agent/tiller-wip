package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/fnv"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
)

type Manager struct {
	db       *sql.DB
	registry *Registry
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
}

func NewManager(db *sql.DB, registry *Registry) *Manager {
	return &Manager{db: db, registry: registry, locks: make(map[string]*sync.Mutex)}
}

func (m *Manager) Registry() *Registry { return m.registry }

func (m *Manager) Refresh(ctx context.Context, providerID string) error {
	lock := m.providerLock(providerID)
	lock.Lock()
	defer lock.Unlock()
	provider, err := m.loadProvider(ctx, providerID)
	if err != nil {
		return err
	}
	models, discoverErr := m.registry.Discover(ctx, provider)
	if discoverErr != nil {
		// Diagnostic only (no behavior change): surface the exact discovery
		// error so CI failures like the browser-suite "initial discovery
		// failed." flake are self-identifying instead of guessing. Writes to
		// the default slog logger (stderr) -> lands in `docker logs router`.
		slog.Error("tiller: provider discovery failed",
			"provider_id", providerID,
			"provider", provider.Name,
			"base_url", provider.BaseURL,
			"error", discoverErr,
		)
		_, storeErr := m.db.ExecContext(ctx, `UPDATE providers SET last_refresh_error=?,updated_at=? WHERE id=?`, safeRefreshError(discoverErr), database.Now(), providerID)
		if storeErr != nil {
			return storeErr
		}
		return discoverErr
	}
	if err := m.applyCatalogue(ctx, providerID, models); err != nil {
		return err
	}
	return nil
}

func (m *Manager) loadProvider(ctx context.Context, providerID string) (Instance, error) {
	var p Instance
	var protocols string
	err := m.db.QueryRowContext(ctx, `SELECT id,name,type,base_url,coalesce(credential_secret,''),enabled,protocols FROM providers WHERE id=?`, providerID).
		Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.Credential, &p.Enabled, &protocols)
	p.Protocols = DecodeProtocols(protocols)
	return p, err
}

func (m *Manager) applyCatalogue(ctx context.Context, providerID string, models []Model) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := database.Now()

	// Deduplicate by upstream model id while preserving order. Discovery can
	// legitimately return the same id twice (e.g. duplicated across paged
	// responses), and we want exactly one row per upstream id.
	unique := make([]Model, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if model.ID == "" || seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		unique = append(unique, model)
	}

	// Fetch existing model rows for this provider in a single query so we can
	// reuse their primary keys on conflict. This avoids an N-statement lookup
	// inside the loop and lets the upsert be one statement regardless of model
	// count.
	existing := make(map[string]string, len(unique))
	rows, err := tx.QueryContext(ctx, `SELECT id,upstream_model_id FROM provider_models WHERE provider_id=?`, providerID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var modelID, upstream string
		if err := rows.Scan(&modelID, &upstream); err != nil {
			rows.Close()
			return err
		}
		existing[upstream] = modelID
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Allocate primary keys up front so a single batched UPSERT can carry the
	// correct id for both new (freshly allocated) and existing rows. SQLite's
	// ON CONFLICT requires the inserted id to match the existing row's id when
	// we want to UPDATE non-key columns, so we pre-compute both.
	ids := make([]string, len(unique))
	newIDs := make([]string, 0, len(unique))
	for i, model := range unique {
		if modelID, ok := existing[model.ID]; ok {
			ids[i] = modelID
			continue
		}
		newID, err := id.New()
		if err != nil {
			return err
		}
		ids[i] = newID
		newIDs = append(newIDs, newID)
	}

	// One batched UPSERT for the entire catalogue. The DO UPDATE branch keeps
	// the row's id stable (re-asserting the same value is a no-op) and refreshes
	// every metadata field plus available=1 / last_seen_at. Previously this was
	// O(N) INSERT-or-UPDATE statements inside the transaction.
	if len(unique) > 0 {
		const upsertColumns = 17
		placeholders := make([]string, 0, len(unique))
		args := make([]any, 0, len(unique)*upsertColumns)
		for i, model := range unique {
			placeholders = append(placeholders, "(?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?)")
			args = append(args,
				ids[i], providerID, model.ID, model.DisplayName,
				nullableInt(model.ContextLength), nullableInt(model.MaxOutputTokens),
				nullableProtocol(model.NativeProtocol),
				nullableBool(model.SupportsTools), nullableBool(model.SupportsVision),
				nullableBool(model.SupportsReasoning), nullableBool(model.SupportsStructuredOutput),
				nullableJSON(model.InputModalities), nullableJSON(model.OutputModalities),
				now, now, now, now,
			)
		}
		stmt := `INSERT INTO provider_models(id,provider_id,upstream_model_id,display_name,context_length,max_output_tokens,native_protocol,supports_tools,supports_vision,supports_reasoning,supports_structured_output,input_modalities,output_modalities,available,first_seen_at,last_seen_at,created_at,updated_at) VALUES ` +
			strings.Join(placeholders, ",") + `
			ON CONFLICT(provider_id, upstream_model_id) DO UPDATE SET
				display_name=excluded.display_name,
				context_length=excluded.context_length,
				max_output_tokens=excluded.max_output_tokens,
				native_protocol=excluded.native_protocol,
				supports_tools=excluded.supports_tools,
				supports_vision=excluded.supports_vision,
				supports_reasoning=excluded.supports_reasoning,
				supports_structured_output=excluded.supports_structured_output,
				input_modalities=excluded.input_modalities,
				output_modalities=excluded.output_modalities,
				available=1,
				last_seen_at=excluded.last_seen_at,
				updated_at=excluded.updated_at`
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
	}

	// Seed default permissions for newly-discovered models in a single statement
	// per model. Each row creates one entry per client_key, mirroring the prior
	// per-row INSERT. client_group_defaults is read with a LEFT JOIN so clients
	// without a default row fall back to enabled=0 (matches prior behaviour).
	for _, newID := range newIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at)
			SELECT c.id,'real',?,coalesce(d.new_models_enabled,0),?,? FROM client_keys c LEFT JOIN client_group_defaults d ON d.client_key_id=c.id AND d.group_kind='real' AND d.group_id=?`, newID, now, now, providerID); err != nil {
			return err
		}
	}

	// Mark models that disappeared from the catalogue as unavailable in one
	// statement using NOT IN. The IN list is always non-empty here: if discovery
	// returned zero models, seen is empty and the earlier branch above would
	// have used the simpler bulk update instead. Wait — that's not true when
	// every model was a duplicate that was filtered out. Guard with a length
	// check so we never pass an empty IN list to SQLite.
	if len(seen) > 0 {
		placeholders := make([]string, 0, len(seen))
		args := make([]any, 0, len(seen)+1)
		args = append(args, providerID)
		for upstream := range seen {
			placeholders = append(placeholders, "?")
			args = append(args, upstream)
		}
		stmt := `UPDATE provider_models SET available=0,updated_at=? WHERE provider_id=? AND upstream_model_id NOT IN (` + strings.Join(placeholders, ",") + `)`
		// Prepend `now` to the args since the WHERE clause order is provider_id-first.
		fullArgs := make([]any, 0, len(args)+1)
		fullArgs = append(fullArgs, now)
		fullArgs = append(fullArgs, args...)
		if _, err := tx.ExecContext(ctx, stmt, fullArgs...); err != nil {
			return err
		}
	} else {
		// Discovery returned no usable models. Retire everything for this provider.
		if _, err := tx.ExecContext(ctx, `UPDATE provider_models SET available=0,updated_at=? WHERE provider_id=?`, now, providerID); err != nil {
			return err
		}
	}

	next := time.Now().UTC().Add(24*time.Hour + refreshJitter(providerID)).Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE providers SET last_refresh_at=?,next_refresh_at=?,last_refresh_error=NULL,updated_at=? WHERE id=?`, now, next, now, providerID); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) StartScheduler(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.refreshDue(ctx)
			}
		}
	}()
}

func (m *Manager) refreshDue(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `SELECT id FROM providers WHERE enabled=1 AND (next_refresh_at IS NULL OR next_refresh_at<=?)`, database.Now())
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var providerID string
		if rows.Scan(&providerID) == nil {
			ids = append(ids, providerID)
		}
	}
	rows.Close()
	for _, providerID := range ids {
		go func(value string) {
			refreshCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			_ = m.Refresh(refreshCtx, value)
		}(providerID)
	}
}

func (m *Manager) providerLock(providerID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock := m.locks[providerID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[providerID] = lock
	}
	return lock
}

func refreshJitter(providerID string) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(providerID))
	return time.Duration(int64(h.Sum32()%7200)-3600) * time.Second
}

var discoveryHTTPStatus = regexp.MustCompile(`^model discovery returned HTTP ([0-9]{3})$`)

func safeRefreshError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "Provider discovery timed out."
	}
	if errors.Is(err, context.Canceled) {
		return "Provider discovery was cancelled."
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return "Provider discovery timed out."
		}
		return "Provider discovery request failed."
	}
	if match := discoveryHTTPStatus.FindStringSubmatch(err.Error()); len(match) == 2 {
		return "Provider discovery returned HTTP " + match[1] + "."
	}
	return "Provider discovery failed."
}

func nullableInt(v int) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullableProtocol(v Protocol) any {
	if v == "" {
		return nil
	}
	return string(v)
}

func nullableBool(v *bool) any {
	if v == nil {
		return nil
	}
	if *v {
		return 1
	}
	return 0
}

func nullableJSON(list []string) any {
	if len(list) == 0 {
		return nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return nil
	}
	return string(b)
}
