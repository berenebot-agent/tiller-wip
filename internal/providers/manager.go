package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/providers/claude"
	"github.com/tiller-router/tiller-router/internal/providers/codex"
	"github.com/tiller-router/tiller-router/internal/providers/github"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
	"github.com/tiller-router/tiller-router/internal/store"
)

// ErrManualModelExists is returned when a manual model would duplicate an
// existing (provider_id, upstream_model_id) row.
var ErrManualModelExists = store.ErrManualModelExists

// ErrProviderNotFound is returned when a provider does not exist in the
// account.
var ErrProviderNotFound = store.ErrProviderNotFound

type Manager struct {
	store    *store.Store
	registry *Registry
	oauth    *oauth.Manager
	logger   *slog.Logger
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
}

// refreshConcurrency bounds how many provider discoveries the scheduler runs at
// once. Discovery is outbound network work against a provider-supplied endpoint,
// so a tick that finds many providers due at the same moment is throttled to
// this many in flight; the rest queue behind a slot.
const refreshConcurrency = 4

func NewManager(st *store.Store, registry *Registry, logger *slog.Logger) *Manager {
	return &Manager{store: st, registry: registry, oauth: oauth.NewManager(st, 5*time.Minute, logger), logger: logger, locks: make(map[string]*sync.Mutex)}
}

func (m *Manager) Registry() *Registry { return m.registry }

// ForceOAuthRefresh forces a token refresh for an OAuth provider and updates
// the instance credential in place. Returns ErrReconnectRequired when the
// refresh token is dead, ErrAuthUnavailable on transient failure, or nil on
// success. Non-OAuth providers return an error immediately.
func (m *Manager) ForceOAuthRefresh(ctx context.Context, accountID string, p *Instance) error {
	refresh := m.oauthRefreshFunc(p.Type)
	if refresh == nil {
		return errors.New("not an oauth provider")
	}
	record, err := m.oauth.RefreshRejected(ctx, accountID, p.ID, refresh, p.Credential)
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

func (m *Manager) Refresh(ctx context.Context, accountID, providerID string) error {
	// Load first: the per-provider lock is only dropped on a successful delete,
	// so taking it for a provider that does not exist (a refresh of a bogus id)
	// would leave a permanent entry in the map.
	_, err := m.loadProvider(ctx, accountID, providerID)
	if err != nil {
		return err
	}
	lock := m.providerLock(accountID, providerID)
	lock.Lock()
	defer lock.Unlock()
	// Reload under the lock: the existence check above only bounds the lock map,
	// and a concurrent refresh or admin edit may have changed the endpoint or
	// credential since it ran. Discovering with a stale instance would apply a
	// catalogue fetched from the old base URL.
	provider, err := m.loadProvider(ctx, accountID, providerID)
	if err != nil {
		return err
	}
	models, err := m.discover(ctx, accountID, provider)
	if err != nil {
		message := safeRefreshError(err)
		if m.logger != nil && !errors.Is(err, context.Canceled) {
			m.logger.WarnContext(ctx, "provider catalogue refresh failed", "account_id", accountID,
				"provider_id", providerID, "provider_type", provider.Type, "reason", message)
		}
		_ = m.store.For(accountID).SetProviderRefreshError(ctx, providerID, message)
		return err
	}
	return m.store.For(accountID).ApplyCatalogue(ctx, providerID, toCatalogueModels(models))
}

func (m *Manager) discover(ctx context.Context, accountID string, provider Instance) ([]Model, error) {
	if err := m.HydrateOAuth(ctx, accountID, &provider); err != nil {
		return nil, err
	}
	models, err := m.registry.Discover(ctx, provider)
	var status discoveryStatusError
	if provider.OAuthState != string(oauth.AuthConnected) || !errors.As(err, &status) ||
		(status != http.StatusUnauthorized && status != http.StatusForbidden) {
		return models, err
	}
	if err := m.ForceOAuthRefresh(ctx, accountID, &provider); err != nil {
		return nil, err
	}
	return m.registry.Discover(ctx, provider)
}

func (m *Manager) loadProvider(ctx context.Context, accountID, providerID string) (Instance, error) {
	row, err := m.store.For(accountID).LoadProvider(ctx, providerID)
	if err != nil {
		return Instance{}, err
	}
	p := Instance{
		ID:         row.ID,
		Name:       row.Name,
		Type:       row.Type,
		BaseURL:    row.BaseURL,
		Credential: row.Credential,
		Enabled:    row.Enabled,
	}
	p.Protocols = DecodeProtocols(row.Protocols)
	if d, ok := Lookup(p.Type); ok {
		p.MinOutputTokens = d.MinOutputTokens
	}
	return p, nil
}

// HydrateOAuth loads the current access token only for OAuth descriptors. It
// deliberately leaves API-key credentials untouched. On success it sets
// p.Credential and p.OAuthProviderData; on failure it leaves p.Credential empty
// and sets p.OAuthState to the classified auth state so the routing layer can
// distinguish "not connected", "refresh failed", and "reconnect required".
func (m *Manager) HydrateOAuth(ctx context.Context, accountID string, p *Instance) error {
	descriptor, ok := Lookup(p.Type)
	if !ok || descriptor.AuthMode != AuthModeOAuth {
		return nil
	}
	p.Credential = ""
	token, err := m.oauth.Current(ctx, accountID, p.ID, m.oauthRefreshFunc(p.Type))
	if err != nil {
		switch {
		case errors.Is(err, oauth.ErrNoToken):
			p.OAuthState = "not_connected"
		case errors.Is(err, oauth.ErrReconnectRequired):
			p.OAuthState = string(oauth.AuthReconnectRequired)
		default:
			p.OAuthState = string(oauth.AuthUnavailable)
		}
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
func (m *Manager) ResolveManualModel(ctx context.Context, accountID, providerID, upstreamID string) (Model, error) {
	provider, err := m.loadProvider(ctx, accountID, providerID)
	if err != nil {
		return Model{}, err
	}
	model := Model{ID: upstreamID}
	probeCtx, cancel := context.WithTimeout(ctx, manualModelProbeTimeout)
	discovered, discoverErr := m.discover(probeCtx, accountID, provider)
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
func (m *Manager) AddManualModel(ctx context.Context, accountID, providerID string, in ManualModelInput) (string, error) {
	model, err := m.ResolveManualModel(ctx, accountID, providerID, in.UpstreamModelID)
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
	cm := toCatalogueModels([]Model{model})[0]
	if err := m.store.For(accountID).InsertManualModel(ctx, providerID, modelID, cm); err != nil {
		return "", err
	}
	return modelID, nil
}

func toCatalogueModels(models []Model) []store.CatalogueModel {
	out := make([]store.CatalogueModel, 0, len(models))
	for _, m := range models {
		cm := store.CatalogueModel{
			ID:                       m.ID,
			DisplayName:              m.DisplayName,
			NativeProtocol:           string(m.NativeProtocol),
			SupportsTools:            m.SupportsTools,
			SupportsVision:           m.SupportsVision,
			SupportsReasoning:        m.SupportsReasoning,
			SupportsStructuredOutput: m.SupportsStructuredOutput,
			SupportsTemperature:      m.SupportsTemperature,
			InputModalities:          m.InputModalities,
			OutputModalities:         m.OutputModalities,
		}
		if m.ContextLength > 0 {
			v := m.ContextLength
			cm.ContextLength = &v
		}
		if m.MaxOutputTokens > 0 {
			v := m.MaxOutputTokens
			cm.MaxOutputTokens = &v
		}
		if raw := nullableReasoningCapabilities(m.ReasoningCapabilities); raw != nil {
			if s, ok := raw.(string); ok {
				cm.ReasoningCapabilities = json.RawMessage(s)
			}
		}
		out = append(out, cm)
	}
	return out
}

func (m *Manager) StartScheduler(ctx context.Context) {
	go func() {
		timer := time.NewTimer(15 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				// Wait for the entire batch, then schedule the next pass.
				// No two background batches can overlap.
				m.refreshDue(ctx)
				if ctx.Err() != nil {
					return
				}
				timer.Reset(15 * time.Minute)
			}
		}
	}()
}

// refreshDue processes providers due for discovery with a fixed worker pool.
// There are at most refreshConcurrency background requests in flight, even if
// thousands of providers are due. Work waiting for a worker uses no goroutine
// of its own, and its three-minute timeout begins only when work starts.
// Manual Refresh calls bypass this queue and retain the provider-level lock.
func (m *Manager) refreshDue(ctx context.Context) {
	refs, err := m.store.DueProviders(ctx, database.Now())
	if err != nil || len(refs) == 0 || ctx.Err() != nil {
		return
	}

	jobs := make(chan store.ProviderRef)
	var workers sync.WaitGroup
	for i := 0; i < refreshConcurrency && i < len(refs); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case ref, ok := <-jobs:
					if !ok || ctx.Err() != nil {
						return
					}
					refreshCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
					_ = m.Refresh(refreshCtx, ref.AccountID, ref.ProviderID)
					cancel()
				}
			}
		}()
	}

	for _, ref := range refs {
		select {
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		case jobs <- ref:
		}
	}
	close(jobs)
	workers.Wait()
}

func (m *Manager) providerLock(accountID, providerID string) *sync.Mutex {
	key := accountID + "\x00" + providerID
	m.mu.Lock()
	defer m.mu.Unlock()
	lock := m.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[key] = lock
	}
	return lock
}

func (m *Manager) DropProviderLock(accountID, providerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.locks, accountID+"\x00"+providerID)
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

func safeRefreshError(err error) string {
	var status discoveryStatusError
	var tokenErr *oauth.TokenEndpointError
	if errors.As(err, &status) {
		return fmt.Sprintf("Provider discovery returned HTTP %d.", status)
	}
	if errors.Is(err, oauth.ErrReconnectRequired) {
		return "Provider OAuth connection needs to be renewed. Reconnect the provider."
	}
	if errors.Is(err, oauth.ErrNoToken) {
		return "Provider OAuth connection is missing. Connect the provider."
	}
	if errors.Is(err, oauth.ErrAuthUnavailable) || errors.As(err, &tokenErr) {
		return "Provider OAuth credentials are temporarily unavailable."
	}
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
	return "Provider discovery failed."
}
