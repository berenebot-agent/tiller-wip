package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/providers/claude"
	"github.com/tiller-router/tiller-router/internal/providers/codex"
	"github.com/tiller-router/tiller-router/internal/providers/github"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

const codexRedirectURI = "http://localhost:1455/auth/callback"

type oauthDeviceState struct {
	Status string
	Device github.DeviceCode
	Token  oauth.TokenRecord
	Err    string
}

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
	if providerType == "github-copilot" {
		device, startErr := s.startGitHubDeviceFlow(r.Context(), id)
		if startErr != nil {
			adminError(w, 502, "oauth_start_failed", "Could not start GitHub OAuth.")
			return
		}
		writeJSON(w, 200, map[string]any{"flow": "device_code", "verification_uri": device.VerificationURI, "verification_uri_complete": device.VerificationURIComplete, "user_code": device.UserCode, "expires_in": device.ExpiresIn, "interval": int(device.Interval / time.Second)})
		return
	}
	if providerType != "codex-subscription" && providerType != "claude-subscription" {
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
	authURL := ""
	if providerType == "codex-subscription" {
		authURL, err = codex.AuthorizationURL(codexRedirectURI, flow.PKCE.State, flow.PKCE.Challenge)
	} else {
		authURL, err = claude.AuthorizationURL(codexRedirectURI, flow.PKCE.State, flow.PKCE.Challenge)
	}
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
	if providerType != "codex-subscription" && providerType != "claude-subscription" {
		adminError(w, 400, "oauth_not_supported", "OAuth is not supported for this provider.")
		return
	}
	var input struct {
		RedirectedURL string `json:"redirected_url"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	callback, err := oauth.ParseCallback(input.RedirectedURL)
	if providerType == "claude-subscription" {
		callback, err = claude.ParseCallback(input.RedirectedURL)
	}
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
	var tokens oauth.TokenResponse
	if providerType == "codex-subscription" {
		tokens, err = codex.Exchange(r.Context(), s.providers.Registry().HTTPClient(), callback.Code, codexRedirectURI, flow.PKCE.Verifier)
	} else {
		tokens, err = claude.Exchange(r.Context(), s.providers.Registry().HTTPClient(), callback.Code, codexRedirectURI, flow.PKCE.Verifier, flow.PKCE.State)
	}
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

func (s *Server) startGitHubDeviceFlow(ctx context.Context, id string) (github.DeviceCode, error) {
	s.oauthDeviceMu.Lock()
	if existing := s.oauthDevices[id]; existing != nil && existing.Status == "pending" {
		device := existing.Device
		s.oauthDeviceMu.Unlock()
		return device, nil
	}
	s.oauthDevices[id] = &oauthDeviceState{Status: "pending"}
	s.oauthDeviceMu.Unlock()
	device, err := github.RequestDeviceCode(ctx, s.providers.Registry().HTTPClient())
	if err != nil {
		s.finishDevice(id, "failed", err)
		return github.DeviceCode{}, err
	}
	s.oauthDeviceMu.Lock()
	if state := s.oauthDevices[id]; state != nil {
		state.Device = device
	}
	s.oauthDeviceMu.Unlock()
	go func() {
		tokens, err := github.PollToken(context.Background(), s.providers.Registry().HTTPClient(), device)
		if err != nil {
			s.finishDevice(id, "failed", err)
			return
		}
		user, _ := github.FetchUser(context.Background(), s.providers.Registry().HTTPClient(), tokens.AccessToken)
		copilot, _, err := github.FetchCopilotToken(context.Background(), s.providers.Registry().HTTPClient(), tokens.AccessToken)
		if err != nil {
			s.finishDevice(id, "failed", err)
			return
		}
		for key, value := range copilot.ProviderData {
			if tokens.ProviderData == nil {
				tokens.ProviderData = map[string]any{}
			}
			tokens.ProviderData[key] = value
		}
		tokens.ExpiresIn = copilot.ExpiresIn
		tokens.AccountEmail, tokens.AccountPlan = user.Email, user.Login
		record, err := oauth.MergeToken(oauth.TokenRecord{ProviderID: id}, tokens, time.Now().UTC())
		if err != nil {
			s.finishDevice(id, "failed", err)
			return
		}
		if err := oauth.NewStore(s.db.SQL).Put(context.Background(), record); err != nil {
			s.finishDevice(id, "failed", err)
			return
		}
		s.oauthDeviceMu.Lock()
		if state := s.oauthDevices[id]; state != nil {
			state.Status, state.Token = "connected", record
		}
		s.oauthDeviceMu.Unlock()
	}()
	return device, nil
}

func (s *Server) finishDevice(id, status string, err error) {
	s.oauthDeviceMu.Lock()
	defer s.oauthDeviceMu.Unlock()
	if state := s.oauthDevices[id]; state != nil {
		state.Status = status
		if err != nil {
			state.Err = "GitHub OAuth connection failed."
		}
	}
}

func (s *Server) providerOAuthStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.oauthDeviceMu.Lock()
	state := s.oauthDevices[id]
	s.oauthDeviceMu.Unlock()
	if state != nil {
		result := map[string]any{"status": state.Status}
		if state.Device.VerificationURI != "" {
			result["verification_uri"] = state.Device.VerificationURI
			result["verification_uri_complete"] = state.Device.VerificationURIComplete
			result["user_code"] = state.Device.UserCode
			result["expires_in"] = state.Device.ExpiresIn
		}
		if state.Err != "" {
			result["error"] = state.Err
		}
		writeJSON(w, 200, result)
		return
	}
	record, err := oauth.NewStore(s.db.SQL).Get(r.Context(), id)
	if err == oauth.ErrNoToken {
		writeJSON(w, 200, map[string]any{"status": "none"})
		return
	}
	if err != nil {
		adminError(w, 500, "database_error", "Could not load OAuth status.")
		return
	}
	result := map[string]any{"status": string(oauth.Classify(record, time.Now().UTC()))}
	if record.AccountEmail != "" {
		result["account_email"] = record.AccountEmail
	}
	if record.AccountPlan != "" {
		result["account_plan"] = record.AccountPlan
	}
	writeJSON(w, 200, result)
}

func (s *Server) oauthProviderType(ctx context.Context, id string) (string, error) {
	var providerType string
	err := s.db.SQL.QueryRowContext(ctx, `SELECT type FROM providers WHERE id=?`, id).Scan(&providerType)
	return providerType, err
}

// disconnectProviderOAuth removes the OAuth token and in-memory state for a
// provider while preserving the provider configuration, models, and routing.
// It is idempotent: disconnecting an already-disconnected provider succeeds.
func (s *Server) disconnectProviderOAuth(w http.ResponseWriter, r *http.Request) {
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
	if descriptor, ok := providers.Lookup(providerType); !ok || descriptor.AuthMode != providers.AuthModeOAuth {
		adminError(w, 400, "oauth_not_supported", "OAuth is not supported for this provider.")
		return
	}
	if err := oauth.NewStore(s.db.SQL).Delete(r.Context(), id); err != nil {
		adminError(w, 500, "database_error", "Could not remove OAuth connection.")
		return
	}
	s.oauthDeviceMu.Lock()
	delete(s.oauthDevices, id)
	s.oauthDeviceMu.Unlock()
	s.oauthFlows.Cancel(id)
	writeJSON(w, 200, map[string]any{"status": "disconnected"})
}
