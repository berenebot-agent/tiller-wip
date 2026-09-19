package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"regexp"
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
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
}

func NewManager(db *sql.DB, registry *Registry) *Manager {
	st := store.New(db)
	return &Manager{store: st, registry: registry, oauth: oauth.NewManager(st, 5*time.Minute), locks: make(map[string]*sync.Mutex)}
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
	record, err := m.oauth.ForceRefresh(ctx, accountID, p.ID, refresh)
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
	lock := m.providerLock(accountID, providerID)
	lock.Lock()
	defer lock.Unlock()
	provider, err := m.loadProvider(ctx, accountID, providerID)
	if err != nil {
		return err
	}
	models, discoverErr := m.registry.Discover(ctx, provider)
	if discoverErr != nil {
		_ = m.store.For(accountID).SetProviderRefreshError(ctx, providerID, safeRefreshError(discoverErr))
		return discoverErr
	}
	return m.store.For(accountID).ApplyCatalogue(ctx, providerID, toCatalogueModels(models))
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
	m.HydrateOAuth(ctx, accountID, &p)
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
	var refresh oauth.RefreshFunc
	switch p.Type {
	case codexProviderType:
		refresh = func(refreshCtx context.Context, current oauth.TokenRecord) (oauth.TokenResponse, error) {
			return codex.Refresh(refreshCtx, m.registry.HTTPClient(), current.RefreshToken)
		}
	case "claude-subscription":
		refresh = func(refreshCtx context.Context, current oauth.TokenRecord) (oauth.TokenResponse, error) {
			return claude.Refresh(refreshCtx, m.registry.HTTPClient(), current.RefreshToken)
		}
	case "github-copilot":
		refresh = func(refreshCtx context.Context, current oauth.TokenRecord) (oauth.TokenResponse, error) {
			return github.Refresh(refreshCtx, m.registry.HTTPClient(), current)
		}
	}
	token, err := m.oauth.Current(ctx, accountID, p.ID, refresh)
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
	refs, err := m.store.DueProviders(ctx, database.Now())
	if err != nil {
		return
	}
	for _, ref := range refs {
		go func(accountID, providerID string) {
			refreshCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			_ = m.Refresh(refreshCtx, accountID, providerID)
		}(ref.AccountID, ref.ProviderID)
	}
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
