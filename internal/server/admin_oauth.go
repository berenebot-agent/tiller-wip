package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/tiller-router/tiller-router/internal/providers/codex"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

const codexRedirectURI = "http://localhost:1455/auth/callback"

func (s *Server) startProviderOAuth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	providerType, err := s.oauthProviderType(r.Context(), id)
	if err == sql.ErrNoRows {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	}
	if err != nil {
		adminError(w, 500, "database_error", "Could not load provider.")
		return
	}
	if providerType != "codex-subscription" {
		adminError(w, 400, "oauth_not_supported", "OAuth is not supported for this provider.")
		return
	}
	flow, err := s.oauthFlows.Begin(id)
	if errors.Is(err, oauth.ErrFlowActive) {
		adminError(w, 409, "oauth_flow_active", "An OAuth connection is already in progress.")
		return
	}
	if err != nil {
		adminError(w, 500, "oauth_start_failed", "Could not start OAuth connection.")
		return
	}
	authURL, err := codex.AuthorizationURL(codexRedirectURI, flow.PKCE.State, flow.PKCE.Challenge)
	if err != nil {
		adminError(w, 500, "oauth_start_failed", "Could not build OAuth authorization URL.")
		return
	}
	writeJSON(w, 200, map[string]any{"authorization_url": authURL, "redirect_uri": codexRedirectURI, "expires_in": int((10 * time.Minute) / time.Second)})
}

func (s *Server) completeProviderOAuth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	providerType, err := s.oauthProviderType(r.Context(), id)
	if err == sql.ErrNoRows {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	}
	if err != nil {
		adminError(w, 500, "database_error", "Could not load provider.")
		return
	}
	if providerType != "codex-subscription" {
		adminError(w, 400, "oauth_not_supported", "OAuth is not supported for this provider.")
		return
	}
	var input struct{ RedirectedURL string `json:"redirected_url"` }
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	callback, err := oauth.ParseCallback(input.RedirectedURL)
	if err != nil {
		adminError(w, 400, "invalid_oauth_callback", "Paste the complete redirected callback URL.")
		return
	}
	flow, err := s.oauthFlows.Consume(id, callback.State)
	if err != nil {
		adminError(w, 400, "invalid_oauth_state", "This OAuth callback is invalid, expired, or already used.")
		return
	}
	if r.Context().Err() != nil {
		return
	}
	tokens, err := codex.Exchange(r.Context(), s.providers.Registry().HTTPClient(), callback.Code, codexRedirectURI, flow.PKCE.Verifier)
	if err != nil {
		adminError(w, 502, "oauth_exchange_failed", "OAuth token exchange failed.")
		return
	}
	record, err := oauth.MergeToken(oauth.TokenRecord{ProviderID: id}, tokens, time.Now().UTC())
	if err != nil {
		adminError(w, 502, "oauth_exchange_failed", "OAuth token exchange returned an invalid token.")
		return
	}
	if err := oauth.NewStore(s.db.SQL).Put(context.Background(), record); err != nil {
		adminError(w, 500, "database_error", "Could not save OAuth connection.")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "connected", "account_email": record.AccountEmail, "account_plan": record.AccountPlan})
}

func (s *Server) oauthProviderType(ctx context.Context, id string) (string, error) {
	var providerType string
	err := s.db.SQL.QueryRowContext(ctx, `SELECT type FROM providers WHERE id=?`, id).Scan(&providerType)
	return providerType, err
}
