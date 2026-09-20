package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/providers/claude"
	"github.com/tiller-router/tiller-router/internal/providers/codex"
	"github.com/tiller-router/tiller-router/internal/providers/github"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
	"github.com/tiller-router/tiller-router/internal/store"
)

const oauthRedirectPath = "/auth/callback"

func (s *Server) oauthRedirectURI(r *http.Request) string {
	scheme := "http"
	if s.secureRequest(r) {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: r.Host, Path: oauthRedirectPath}).String()
}

func (s *Server) oauthRateLimited(w http.ResponseWriter, r *http.Request, limiter *loginLimiter) bool {
	key := clientIP(r, s.config.TrustedProxy)
	if limiter.locked(key) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many OAuth requests. Try again later.")
		return true
	}
	return false
}

func (s *Server) recordOAuthFailure(r *http.Request, limiter *loginLimiter) bool {
	return limiter.recordFailure(clientIP(r, s.config.TrustedProxy))
}

type oauthDeviceState struct {
	Status string
	Device github.DeviceCode
	Token  oauth.TokenRecord
	Err    string
	Cancel context.CancelFunc
}

func (s *Server) startProviderOAuth(w http.ResponseWriter, r *http.Request) {
	if s.oauthRateLimited(w, r, s.oauthStartLimiter) {
		return
	}
	id := r.PathValue("id")
	providerType, err := s.scope(r).ProviderType(r.Context(), id)
	if errors.Is(err, store.ErrProviderNotFound) {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	}
	if err != nil {
		adminError(w, 500, "database_error", "Could not load provider.")
		return
	}
	if providerType == "github-copilot" {
		device, startErr := s.startGitHubDeviceFlow(r.Context(), s.scope(r).AccountID(), id)
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
	redirectURI := s.oauthRedirectURI(r)
	scope := s.scope(r)
	generation, err := scope.OAuthGeneration(r.Context(), id)
	if err != nil {
		adminError(w, 500, "database_error", "Could not start OAuth connection.")
		return
	}
	flow, err := s.oauthFlows.BeginWithGeneration(scope.AccountID(), id, redirectURI, generation)
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
		authURL, err = codex.AuthorizationURL(redirectURI, flow.PKCE.State, flow.PKCE.Challenge)
	} else {
		authURL, err = claude.AuthorizationURL(redirectURI, flow.PKCE.State, flow.PKCE.Challenge)
	}
	if err != nil {
		adminError(w, 500, "oauth_start_failed", "Could not build OAuth authorization URL.")
		return
	}
	writeJSON(w, 200, map[string]any{"authorization_url": authURL, "redirect_uri": redirectURI, "expires_in": int((10 * time.Minute) / time.Second)})
}

func (s *Server) completeProviderOAuth(w http.ResponseWriter, r *http.Request) {
	if s.oauthRateLimited(w, r, s.oauthCallbackLimiter) {
		return
	}
	id := r.PathValue("id")
	providerType, err := s.scope(r).ProviderType(r.Context(), id)
	if errors.Is(err, store.ErrProviderNotFound) {
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
		if s.recordOAuthFailure(r, s.oauthCallbackLimiter) {
			adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many OAuth requests. Try again later.")
			return
		}
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	callback, err := oauth.ParseCallback(input.RedirectedURL)
	if providerType == "claude-subscription" {
		callback, err = claude.ParseCallback(input.RedirectedURL)
	}
	if err != nil {
		if s.recordOAuthFailure(r, s.oauthCallbackLimiter) {
			adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many OAuth requests. Try again later.")
			return
		}
		adminError(w, 400, "invalid_oauth_callback", "Paste the complete redirected callback URL.")
		return
	}
	flow, err := s.oauthFlows.Consume(s.scope(r).AccountID(), id, callback.State)
	if err != nil {
		if s.recordOAuthFailure(r, s.oauthCallbackLimiter) {
			adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many OAuth requests. Try again later.")
			return
		}
		adminError(w, 400, "invalid_oauth_state", "This OAuth callback is invalid, expired, or already used.")
		return
	}
	if r.Context().Err() != nil {
		return
	}
	if flow.RedirectURI == "" {
		adminError(w, 502, "oauth_exchange_failed", "OAuth connection state is invalid.")
		return
	}
	var tokens oauth.TokenResponse
	if providerType == "codex-subscription" {
		tokens, err = codex.Exchange(r.Context(), s.providers.Registry().HTTPClient(), callback.Code, flow.RedirectURI, flow.PKCE.Verifier)
	} else {
		tokens, err = claude.Exchange(r.Context(), s.providers.Registry().HTTPClient(), callback.Code, flow.RedirectURI, flow.PKCE.Verifier, flow.PKCE.State)
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
	scope := s.scope(r)
	record.Generation = flow.Generation
	if err := scope.PutOAuthTokenIfGeneration(r.Context(), oauth.TokenToStore(record), flow.Generation); err != nil {
		if errors.Is(err, store.ErrOAuthGenerationChanged) {
			adminError(w, 409, "oauth_disconnected", "OAuth connection was disconnected while it was completing.")
			return
		}
		adminError(w, 500, "database_error", "Could not save OAuth connection.")
		return
	}
	s.oauthCallbackLimiter.success(clientIP(r, s.config.TrustedProxy))
	writeJSON(w, 200, map[string]any{"status": "connected", "account_email": record.AccountEmail, "account_plan": record.AccountPlan})
}

func (s *Server) startGitHubDeviceFlow(ctx context.Context, accountID, id string) (github.DeviceCode, error) {
	generation, err := s.scopeFor(accountID).OAuthGeneration(ctx, id)
	if err != nil {
		return github.DeviceCode{}, err
	}
	s.oauthDeviceMu.Lock()
	if existing := s.oauthDevices[tenantKey(accountID, id)]; existing != nil && existing.Status == "pending" {
		device := existing.Device
		s.oauthDeviceMu.Unlock()
		return device, nil
	}
	flowCtx, cancel := context.WithCancel(s.backgroundCtx)
	s.oauthDevices[tenantKey(accountID, id)] = &oauthDeviceState{Status: "pending", Cancel: cancel}
	s.oauthDeviceMu.Unlock()
	device, err := github.RequestDeviceCode(flowCtx, s.providers.Registry().HTTPClient())
	if err != nil {
		s.finishDevice(accountID, id, "failed", err)
		return github.DeviceCode{}, err
	}
	s.oauthDeviceMu.Lock()
	if state := s.oauthDevices[tenantKey(accountID, id)]; state != nil {
		state.Device = device
	}
	s.oauthDeviceMu.Unlock()
	pollCtx := flowCtx
	go func() {
		tokens, err := github.PollToken(pollCtx, s.providers.Registry().HTTPClient(), device)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				s.finishDevice(accountID, id, "failed", err)
			}
			return
		}
		user, _ := github.FetchUser(pollCtx, s.providers.Registry().HTTPClient(), tokens.AccessToken)
		copilot, _, err := github.FetchCopilotToken(pollCtx, s.providers.Registry().HTTPClient(), tokens.AccessToken)
		if err != nil {
			s.finishDevice(accountID, id, "failed", err)
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
			s.finishDevice(accountID, id, "failed", err)
			return
		}
		record.Generation = generation
		if err := s.scopeFor(accountID).PutOAuthTokenIfGeneration(pollCtx, oauth.TokenToStore(record), generation); err != nil {
			s.finishDevice(accountID, id, "failed", err)
			return
		}
		s.oauthDeviceMu.Lock()
		if state := s.oauthDevices[tenantKey(accountID, id)]; state != nil {
			state.Status, state.Token = "connected", record
		}
		s.oauthDeviceMu.Unlock()
	}()
	return device, nil
}

func (s *Server) finishDevice(accountID, id, status string, err error) {
	s.oauthDeviceMu.Lock()
	defer s.oauthDeviceMu.Unlock()
	if state := s.oauthDevices[tenantKey(accountID, id)]; state != nil {
		state.Status = status
		if err != nil {
			state.Err = "GitHub OAuth connection failed."
		}
	}
}

func (s *Server) providerOAuthStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.oauthDeviceMu.Lock()
	state := s.oauthDevices[tenantKey(s.scope(r).AccountID(), id)]
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
	row, err := s.scope(r).GetOAuthToken(r.Context(), id)
	if errors.Is(err, store.ErrNoOAuthToken) {
		writeJSON(w, 200, map[string]any{"status": "none"})
		return
	}
	if err != nil {
		adminError(w, 500, "database_error", "Could not load OAuth status.")
		return
	}
	record := oauth.TokenFromStore(row)
	result := map[string]any{"status": string(oauth.Classify(record, time.Now().UTC()))}
	if record.AccountEmail != "" {
		result["account_email"] = record.AccountEmail
	}
	if record.AccountPlan != "" {
		result["account_plan"] = record.AccountPlan
	}
	writeJSON(w, 200, result)
}

// disconnectProviderOAuth removes the OAuth token and in-memory state for a
// provider while preserving the provider configuration, models, and routing.
// It is idempotent: disconnecting an already-disconnected provider succeeds.
func (s *Server) disconnectProviderOAuth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	providerType, err := s.scope(r).ProviderType(r.Context(), id)
	if errors.Is(err, store.ErrProviderNotFound) {
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
	scope := s.scope(r)
	if _, err := scope.AdvanceOAuthGeneration(r.Context(), id); err != nil {
		adminError(w, 500, "database_error", "Could not remove OAuth connection.")
		return
	}
	if err := scope.DeleteOAuthToken(r.Context(), id); err != nil {
		adminError(w, 500, "database_error", "Could not remove OAuth connection.")
		return
	}
	s.oauthDeviceMu.Lock()
	if state := s.oauthDevices[tenantKey(scope.AccountID(), id)]; state != nil && state.Cancel != nil {
		state.Cancel()
	}
	delete(s.oauthDevices, tenantKey(scope.AccountID(), id))
	s.oauthDeviceMu.Unlock()
	s.oauthFlows.Cancel(s.scope(r).AccountID(), id)
	writeJSON(w, 200, map[string]any{"status": "disconnected"})
}
