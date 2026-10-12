package server

import (
	"context"
	"net/http"
	"time"

	"github.com/tiller-router/tiller-router/internal/providerquota"
	"github.com/tiller-router/tiller-router/internal/providers"
)

// quotaProviderTypes lists the provider types the quota poller supports. It is
// derived from providerquota.Supports at registration time; the store query
// needs the explicit list so it does not embed provider knowledge.
func quotaProviderTypes() []string {
	// Keep this in step with providerquota's adapter table. It is a small,
	// explicit list by design (no name-shape inference).
	return []string{
		"codex-subscription",
		"claude-subscription",
		"github-copilot",
		"zai",
		"ollama-cloud",
		"commandcode",
	}
}

// hydrateQuotaCredential resolves the current credential for a provider so the
// poller can call its quota endpoint. OAuth providers get a refreshed token;
// API-key providers use their stored credential. The provider type is used to
// select the OAuth hydration path.
func (s *Server) hydrateQuotaCredential(ctx context.Context, ref providerquota.ProviderRef) (string, error) {
	sc := s.scopeFor(ref.AccountID)
	row, err := sc.LoadProvider(ctx, ref.ProviderID)
	if err != nil {
		return "", err
	}
	p := providers.Instance{
		ID:         row.ID,
		Name:       row.Name,
		Type:       row.Type,
		BaseURL:    row.BaseURL,
		Credential: row.Credential,
		Enabled:    row.Enabled,
	}
	if desc, ok := providers.Lookup(p.Type); ok && desc.AuthMode == providers.AuthModeOAuth {
		if err := s.providers.HydrateOAuth(ctx, ref.AccountID, &p); err != nil {
			return "", err
		}
	}
	if s.quota != nil {
		s.quota.SetAccountHeader(ref.AccountID, ref.ProviderID, p.OAuthAccountID)
	}
	return p.Credential, nil
}

// startQuotaPoller launches the background loop that registers quota-capable
// providers and refreshes them on the cadence (30 min idle, 60 s active). A
// failure to list providers is non-fatal: the poller simply has nothing to do
// until the next tick.
func (s *Server) startQuotaPoller(ctx context.Context) {
	if s.quota == nil {
		return
	}
	// Register the currently eligible providers once at startup, then keep the
	// registry fresh on each tick (providers may be created or disabled at
	// runtime).
	s.registerQuotaProviders(ctx)
	go func() {
		// Poll once on boot so the snapshot cache is warm within seconds rather
		// than waiting a full background interval, or an admin opening the
		// Providers page. It runs in this goroutine (not before it) so a cold
		// provider endpoint cannot delay startup. A provider still shows
		// "loading" until this completes; a transient failure is then retried on
		// the short FailureRetry cadence.
		s.refreshQuotaProviders(ctx)
		ticker := time.NewTicker(providerquota.ActiveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshQuotaProviders(ctx)
			}
		}
	}()
}

// registerQuotaProviders (re)builds the poller's provider registry from the
// store. Called at startup and on demand; it does not poll. It reconciles
// rather than adds: a provider that has been deleted or disabled (or whose type
// no longer has an adapter) is unregistered along with its cached snapshot, so
// /api/admin/quota cannot keep showing a card for a provider that is gone. A
// store error returns early and leaves the existing registry untouched — an
// unavailable database must never be read as "no providers configured".
func (s *Server) registerQuotaProviders(ctx context.Context) {
	refs, err := s.storeHandle().QuotaProviders(ctx, quotaProviderTypes())
	if err != nil {
		return
	}
	out := make([]providerquota.ProviderRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, providerquota.ProviderRef{
			AccountID:  ref.AccountID,
			ProviderID: ref.ProviderID,
			Name:       ref.Name,
			Type:       ref.Type,
			BaseURL:    ref.BaseURL,
		})
	}
	s.quota.Reconcile(out)
}

// refreshQuotaProviders polls every registered provider that is due under the
// cadence.
func (s *Server) refreshQuotaProviders(ctx context.Context) {
	s.registerQuotaProviders(ctx)
	refs, err := s.storeHandle().QuotaProviders(ctx, quotaProviderTypes())
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, ref := range refs {
		pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s.quota.RefreshIfDue(pollCtx, ref.AccountID, ref.ProviderID, false, now)
		cancel()
	}
}

// quotaEndpoint returns the cached quota snapshot for every provider in the
// signed-in account, refreshing the eligible ones immediately subject to the
// 30 s floor. It is admin-gated (registered under the authenticated admin
// group). The immediate-on-view refresh is best-effort: cached data is always
// returned.
func (s *Server) quotaEndpoint(w http.ResponseWriter, r *http.Request) {
	accountID := s.scope(r).AccountID()
	if s.quota == nil {
		writeJSON(w, 200, map[string]any{"providers": map[string]any{}})
		return
	}
	// Register/poll on view so a fresh provider appears even before the next
	// background tick, respecting the MinInterval floor the poller enforces.
	s.registerQuotaProviders(r.Context())
	refs, _ := s.storeHandle().QuotaProviders(r.Context(), quotaProviderTypes())
	now := time.Now().UTC()
	for _, ref := range refs {
		if ref.AccountID != accountID {
			continue
		}
		pollCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		s.quota.RefreshIfDue(pollCtx, ref.AccountID, ref.ProviderID, true, now)
		cancel()
	}
	out := s.quota.SnapshotForAccount(accountID)
	if out == nil {
		out = map[string]providerquota.Snapshot{}
	}
	writeJSON(w, 200, map[string]any{"providers": out})
}

// markProviderActive records that a provider served a request, so the quota
// poller tightens its cadence for that provider. It is a no-op when the poller
// is not configured.
func (s *Server) markProviderActive(accountID, providerID string) {
	if s.quota == nil || providerID == "" {
		return
	}
	s.quota.MarkActive(accountID, providerID, time.Now().UTC())
}
