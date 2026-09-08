package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/providers/claude"
	"github.com/tiller-router/tiller-router/internal/providers/codex"
	"github.com/tiller-router/tiller-router/internal/providers/github"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

type Manager struct {
	db       *sql.DB
	registry *Registry
	oauth    *oauth.Manager
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
}

func NewManager(db *sql.DB, registry *Registry) *Manager {
	return &Manager{db: db, registry: registry, oauth: oauth.NewManager(oauth.NewStore(db), 5*time.Minute), locks: make(map[string]*sync.Mutex)}
}

func (m *Manager) Registry() *Registry { return m.registry }

// ForceOAuthRefresh forces a token refresh for an OAuth provider and updates
// the instance credential in place. Returns ErrReconnectRequired when the
// refresh token is dead, ErrAuthUnavailable on transient failure, or nil on
// success. Non-OAuth providers return an error immediately.
func (m *Manager) ForceOAuthRefresh(ctx context.Context, p *Instance) error {
	refresh := m.oauthRefreshFunc(p.Type)
	if refresh == nil {
		return errors.New("not an oauth provider")
	}
	record, err := m.oauth.ForceRefresh(ctx, p.ID, refresh)
	if err == nil {
		p.Credential = record.AccessToken
		p.OAuthProviderData = record.ProviderData
		if p.Type == codexProviderType {
			p.OAuthAccountID = codex.AccountInfo(record.IDToken).ID
		}
	}
	return err
}

func (m *Manager) oauthRefreshFunc(providerType string) oauth.RefreshFunc {
	switch providerType {
	case codexProviderType:
		return func(ctx context.Context, current oauth.TokenRecord) (oauth.TokenResponse, error) {
			return codex.Refresh(ctx, m.registry.HTTPClient(), current.RefreshToken)
		}
	case "claude-subscription":
		return func(ctx context.Context, current oauth.TokenRecord) (oauth.TokenResponse, error) {
			return claude.Refresh(ctx, m.registry.HTTPClient(), current.RefreshToken)
		}
	case "github-copilot":
		return func(ctx context.Context, current oauth.TokenRecord) (oauth.TokenResponse, error) {
			return github.Refresh(ctx, m.registry.HTTPClient(), current)
		}
	}
	return nil
}

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
	if d, ok := Lookup(p.Type); ok {
		p.MinOutputTokens = d.MinOutputTokens
	}
	m.HydrateOAuth(ctx, &p)
	return p, err
}

// HydrateOAuth loads the current access token only for OAuth descriptors. It
// deliberately leaves API-key credentials untouched. On success it sets
// p.Credential and p.OAuthProviderData; on failure it leaves p.Credential empty
// and sets p.OAuthState to the classified auth state so the routing layer can
// distinguish "not connected", "refresh failed", and "reconnect required".
func (m *Manager) HydrateOAuth(ctx context.Context, p *Instance) error {
	descriptor, ok := Lookup(p.Type)
	if !ok || descriptor.AuthMode != AuthModeOAuth {
		return nil
	}
	var token oauth.TokenRecord
	var err error
	if p.Type == codexProviderType {
		token, err = m.oauth.Current(ctx, p.ID, func(refreshCtx context.Context, current oauth.TokenRecord) (oauth.TokenResponse, error) {
			return codex.Refresh(refreshCtx, m.registry.HTTPClient(), current.RefreshToken)
		})
	} else if p.Type == "claude-subscription" {
		token, err = m.oauth.Current(ctx, p.ID, func(refreshCtx context.Context, current oauth.TokenRecord) (oauth.TokenResponse, error) {
			return claude.Refresh(refreshCtx, m.registry.HTTPClient(), current.RefreshToken)
		})
	} else if p.Type == "github-copilot" {
		token, err = m.oauth.Current(ctx, p.ID, func(refreshCtx context.Context, current oauth.TokenRecord) (oauth.TokenResponse, error) {
			return github.Refresh(refreshCtx, m.registry.HTTPClient(), current)
		})
	} else {
		token, err = oauth.NewStore(m.db).Get(ctx, p.ID)
	}
	if err != nil {
		p.OAuthState = string(oauth.Classify(token, time.Now().UTC()))
		return err
	}
	p.Credential = token.AccessToken
	p.OAuthProviderData = token.ProviderData
	p.OAuthState = string(oauth.AuthConnected)
	if p.Type == codexProviderType {
		p.OAuthAccountID = codex.AccountInfo(token.IDToken).ID
	}
	return nil
}

// providerModelInsertColumns is the column list shared by the catalogue upsert
// and single-row manual inserts. available is a literal 1 in
// providerModelInsertPlaceholders, so the bound-variable count is
// providerModelInsertArgs (19), not 20.
const providerModelInsertColumns = "id,provider_id,upstream_model_id,display_name,context_length,max_output_tokens,native_protocol,supports_tools,supports_vision,supports_reasoning,supports_structured_output,input_modalities,output_modalities,reasoning_capabilities,origin,available,first_seen_at,last_seen_at,created_at,updated_at"

const providerModelInsertPlaceholders = "(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?)"

const providerModelInsertArgs = 19

// providerModelArgs returns the bound values for providerModelInsertColumns.
func providerModelArgs(modelID, providerID string, model Model, origin, now string) []any {
	return []any{
		modelID, providerID, model.ID, model.DisplayName,
		nullableInt(model.ContextLength), nullableInt(model.MaxOutputTokens),
		nullableProtocol(model.NativeProtocol),
		nullableBool(model.SupportsTools), nullableBool(model.SupportsVision),
		nullableBool(model.SupportsReasoning), nullableBool(model.SupportsStructuredOutput),
		nullableJSON(model.InputModalities), nullableJSON(model.OutputModalities),
		nullableReasoningCapabilities(model.ReasoningCapabilities),
		origin, now, now, now, now,
	}
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

	// One batched UPSERT for the entire catalogue, chunked to stay under
	// SQLite's variable limit (999). Each row carries 19 bound variables,
	// so batches of 50 keep every statement well under the cap even on
	// large catalogues (previously a single statement broke past ~55 models).
	// The DO UPDATE branch keeps the row's id stable (re-asserting the same
	// value is a no-op) and refreshes every metadata field plus
	// available=1 / last_seen_at. Previously this was O(N) INSERT-or-UPDATE
	// statements inside the transaction.
	if len(unique) > 0 {
		const upsertBatchRows = 50
		for start := 0; start < len(unique); start += upsertBatchRows {
			end := start + upsertBatchRows
			if end > len(unique) {
				end = len(unique)
			}
			placeholders := make([]string, 0, end-start)
			args := make([]any, 0, (end-start)*providerModelInsertArgs)
			for i := start; i < end; i++ {
				placeholders = append(placeholders, providerModelInsertPlaceholders)
				args = append(args, providerModelArgs(ids[i], providerID, unique[i], "discovered", now)...)
			}
			stmt := `INSERT INTO provider_models(` + providerModelInsertColumns + `) VALUES ` +
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
				reasoning_capabilities=excluded.reasoning_capabilities,
				available=1,
				last_seen_at=excluded.last_seen_at,
				updated_at=excluded.updated_at,
				origin=excluded.origin`
			if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
				return err
			}
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

	// Mark models that vanished from the catalogue (or were deduped away) as
	// unavailable. Guard len(seen)>0 so we never build an empty IN (...) list;
	// discovery returning zero usable models falls through to the bulk retire
	// below. The available=1 predicate mirrors the pre-batch behaviour of only
	// retiring rows that were previously available (0->0 is already a no-op,
	// but avoiding the write keeps already-dead rows' updated_at stable).
	// Stale ids are collected in Go memory and retired with chunked IN (...)
	// updates (500 per statement) so no statement exceeds SQLite's 999
	// variable limit, no matter how large the catalogue is.
	if len(seen) > 0 {
		rows, qerr := tx.QueryContext(ctx, `SELECT upstream_model_id FROM provider_models WHERE provider_id=? AND available=1 AND origin='discovered'`, providerID)
		if qerr != nil {
			return qerr
		}
		var stale []string
		for rows.Next() {
			var upstream string
			if serr := rows.Scan(&upstream); serr != nil {
				rows.Close()
				return serr
			}
			if !seen[upstream] {
				stale = append(stale, upstream)
			}
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			return rerr
		}
		const retireBatch = 500
		for start := 0; start < len(stale); start += retireBatch {
			end := start + retireBatch
			if end > len(stale) {
				end = len(stale)
			}
			placeholders := make([]string, 0, end-start)
			rargs := make([]any, 0, (end-start)+2)
			rargs = append(rargs, now, providerID)
			for _, u := range stale[start:end] {
				placeholders = append(placeholders, "?")
				rargs = append(rargs, u)
			}
			if _, uerr := tx.ExecContext(ctx, `UPDATE provider_models SET available=0,updated_at=? WHERE provider_id=? AND available=1 AND origin='discovered' AND upstream_model_id IN (`+strings.Join(placeholders, ",")+`)`, rargs...); uerr != nil {
				return uerr
			}
		}
	} else {
		// Discovery returned no usable models. Retire everything discovered for
		// this provider, but never manual rows.
		if _, err := tx.ExecContext(ctx, `UPDATE provider_models SET available=0,updated_at=? WHERE provider_id=? AND available=1 AND origin='discovered'`, now, providerID); err != nil {
			return err
		}
	}

	next := time.Now().UTC().Add(24*time.Hour + refreshJitter(providerID)).Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE providers SET last_refresh_at=?,next_refresh_at=?,last_refresh_error=NULL,updated_at=? WHERE id=?`, now, next, now, providerID); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrManualModelExists is returned when a manual model would duplicate an
// existing (provider_id, upstream_model_id) row.
var ErrManualModelExists = errors.New("model already exists for provider")

// manualModelProbeTimeout bounds the best-effort live discovery probe run when
// resolving metadata for a manual model, so a slow or unreachable upstream
// cannot stall the admin request indefinitely.
const manualModelProbeTimeout = 30 * time.Second

// ManualModelInput carries the admin-supplied fields for a manual model. Nil
// pointers and empty strings mean "detect"; non-empty values override whatever
// detection finds.
type ManualModelInput struct {
	UpstreamModelID string
	DisplayName     string
	ContextLength   *int64
	MaxOutputTokens *int64
	NativeProtocol  Protocol
}

// ResolveManualModel builds the best available metadata for upstreamID against
// a provider without persisting anything. Live provider discovery is the
// primary source; models.dev fills any gaps (provider data stays authoritative).
// Probe failures are non-fatal: the result degrades to models.dev or unknown.
func (m *Manager) ResolveManualModel(ctx context.Context, providerID, upstreamID string) (Model, error) {
	provider, err := m.loadProvider(ctx, providerID)
	if err != nil {
		return Model{}, err
	}
	model := Model{ID: upstreamID}
	probeCtx, cancel := context.WithTimeout(ctx, manualModelProbeTimeout)
	discovered, discoverErr := m.registry.Discover(probeCtx, provider)
	cancel()
	if discoverErr == nil {
		for _, candidate := range discovered {
			if candidate.ID == upstreamID {
				model = candidate
				break
			}
		}
	}
	if enriched := m.registry.enrich([]Model{model}, provider.Type); len(enriched) == 1 {
		model = enriched[0]
	}
	model.ID = upstreamID
	return model, nil
}

// AddManualModel resolves metadata for a manual model, applies the admin's
// explicit overrides, and persists it with origin='manual'. Manual rows are
// exempt from catalogue retirement until discovery later returns the same id,
// at which point the normal upsert adopts the row as 'discovered'.
func (m *Manager) AddManualModel(ctx context.Context, providerID string, in ManualModelInput) (string, error) {
	model, err := m.ResolveManualModel(ctx, providerID, in.UpstreamModelID)
	if err != nil {
		return "", err
	}
	if in.DisplayName != "" {
		model.DisplayName = in.DisplayName
	}
	if in.ContextLength != nil {
		model.ContextLength = int(*in.ContextLength)
	}
	if in.MaxOutputTokens != nil {
		model.MaxOutputTokens = int(*in.MaxOutputTokens)
	}
	if in.NativeProtocol != "" {
		model.NativeProtocol = in.NativeProtocol
	}
	if model.DisplayName == "" {
		model.DisplayName = in.UpstreamModelID
	}

	modelID, err := id.New()
	if err != nil {
		return "", err
	}
	now := database.Now()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO provider_models(`+providerModelInsertColumns+`) VALUES `+providerModelInsertPlaceholders+` ON CONFLICT(provider_id, upstream_model_id) DO NOTHING`, providerModelArgs(modelID, providerID, model, "manual", now)...)
	if err != nil {
		return "", err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return "", ErrManualModelExists
	}
	// Seed default permissions for every client key, mirroring applyCatalogue.
	if _, err := tx.ExecContext(ctx, `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at)
		SELECT c.id,'real',?,coalesce(d.new_models_enabled,0),?,? FROM client_keys c LEFT JOIN client_group_defaults d ON d.client_key_id=c.id AND d.group_kind='real' AND d.group_id=?`, modelID, now, now, providerID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return modelID, nil
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

func (m *Manager) DropProviderLock(providerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.locks, providerID)
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

func nullableReasoningCapabilities(rc *ReasoningCapabilities) any {
	if rc == nil {
		return nil
	}
	b, err := json.Marshal(rc)
	if err != nil {
		return nil
	}
	return string(b)
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
