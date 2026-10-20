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

// quotaEndpoint returns cached results immediately and initiates due checks
// only for the authenticated account's quota-capable providers.
func (s *Server) quotaEndpoint(w http.ResponseWriter, r *http.Request) {
	accountID := s.scope(r).AccountID()
	if s.quota == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"providers":   map[string]any{},
			"poll_status": map[string]any{},
		})
		return
	}
	refs, err := s.scope(r).QuotaProviders(r.Context(), quotaProviderTypes())
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load provider usage.")
		return
	}
	registered := make([]providerquota.ProviderRef, 0, len(refs))
	for _, ref := range refs {
		registered = append(registered, providerquota.ProviderRef{
			AccountID:  ref.AccountID,
			ProviderID: ref.ProviderID,
			Name:       ref.Name,
			Type:       ref.Type,
			BaseURL:    ref.BaseURL,
		})
	}
	s.quota.ReconcileAccount(accountID, registered)
	now := time.Now().UTC()
	for _, ref := range registered {
		s.quota.TriggerIfDue(accountID, ref.ProviderID, now)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":   s.quota.SnapshotForAccount(accountID),
		"poll_status": s.quota.PollStatesForAccount(accountID),
	})
}
