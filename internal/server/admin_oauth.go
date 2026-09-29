package server

import (
	"context"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/providers/claude"
	"github.com/tiller-router/tiller-router/internal/providers/codex"
	"github.com/tiller-router/tiller-router/internal/providers/github"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
	"github.com/tiller-router/tiller-router/internal/store"
)

const (
	oauthRedirectPath = "/auth/callback"
	oauthPendingTTL   = 10 * time.Minute
)

// markOAuthPending records that a hosted redirect-callback flow has started, so
// status polling reports "pending" rather than a stale pre-existing token.
func (s *Server) markOAuthPending(accountID, providerID string) {
	s.oauthDeviceMu.Lock()
	if s.oauthPending == nil {
		s.oauthPending = map[string]time.Time{}
	}
	s.oauthPending[tenantKey(accountID, providerID)] = time.Now().Add(oauthPendingTTL)
	s.oauthDeviceMu.Unlock()
}

func (s *Server) clearOAuthPending(accountID, providerID string) {
	s.oauthDeviceMu.Lock()
	delete(s.oauthPending, tenantKey(accountID, providerID))
	s.oauthDeviceMu.Unlock()
}

func (s *Server) oauthPendingActive(accountID, providerID string) bool {
	s.oauthDeviceMu.Lock()
	defer s.oauthDeviceMu.Unlock()
	key := tenantKey(accountID, providerID)
	until, ok := s.oauthPending[key]
	if !ok {
		return false
	}
	if !time.Now().Before(until) {
		delete(s.oauthPending, key)
		return false
	}
	return true
}

func (s *Server) oauthRedirectURI(r *http.Request) string {
	// Hosted deployments have a fixed, validated public HTTPS origin, so the
	// redirect URI is canonical and cannot be influenced by the request Host.
	if s.config.Mode == config.ModeHosted && s.config.PublicURL != "" {
		return strings.TrimRight(s.config.PublicURL, "/") + oauthRedirectPath
	}
	scheme := "http"
	if s.secureRequest(r) {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: r.Host, Path: oauthRedirectPath}).String()
}

// oauthRedirectCallback reports whether hosted deployments can complete this
// provider through a server-observed redirect callback rather than paste-back.
// The decision is a provider-scoped compatibility fact (see each provider
// package), never a guess from the model or provider name shape. Codex uses the
// device authorization flow, so it is not part of this path.
func oauthRedirectCallback(providerType string) bool {
	switch providerType {
	case "claude-subscription":
		return claude.RedirectCallbackSupported
	default:
		return false
	}
}

func writeDeviceCodeResponse(w http.ResponseWriter, prompt devicePrompt) {
	writeJSON(w, 200, map[string]any{"flow": "device_code", "verification_uri": prompt.VerificationURI, "verification_uri_complete": prompt.VerificationURIComplete, "user_code": prompt.UserCode, "expires_in": prompt.ExpiresIn, "interval": int(prompt.Interval / time.Second)})
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

// devicePrompt is the provider-neutral subset of a device authorization that
// the admin UI and status endpoint need.
type devicePrompt struct {
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int64
	Interval                time.Duration
}

type oauthDeviceState struct {
	Status     string
	Generation int64
	Prompt     devicePrompt
	Token      oauth.TokenRecord
	Err        string
	Cancel     context.CancelFunc
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
		prompt, startErr := s.startGitHubDeviceFlow(r.Context(), s.scope(r).AccountID(), id)
		if startErr != nil {
			adminError(w, 502, "oauth_start_failed", "Could not start GitHub OAuth.")
			return
		}
		writeDeviceCodeResponse(w, prompt)
		return
	}
	if providerType == "codex-subscription" {
		prompt, startErr := s.startCodexDeviceFlow(r.Context(), s.scope(r).AccountID(), id)
		if errors.Is(startErr, codex.ErrDeviceCodeUnsupported) {
			adminError(w, 502, "oauth_device_code_unsupported", "OpenAI has device sign-in disabled for this account. Try again later.")
			return
		}
		if startErr != nil {
			adminError(w, 502, "oauth_start_failed", "Could not start Codex device sign-in.")
			return
		}
		writeDeviceCodeResponse(w, prompt)
		return
	}
	if providerType != "claude-subscription" {
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
	currentGeneration, err := scope.OAuthGeneration(r.Context(), id)
	if err != nil {
		s.oauthFlows.Cancel(scope.AccountID(), id)
		adminError(w, 500, "database_error", "Could not start OAuth connection.")
		return
	}
	if currentGeneration != generation {
		s.oauthFlows.Cancel(scope.AccountID(), id)
		adminError(w, 409, "oauth_disconnected", "OAuth connection was disconnected while it was starting.")
		return
	}
	authURL, err := claude.AuthorizationURL(redirectURI, flow.PKCE.State, flow.PKCE.Challenge)
	if err != nil {
		adminError(w, 500, "oauth_start_failed", "Could not build OAuth authorization URL.")
		return
	}
	callbackMode := "paste"
	if s.config.Mode == config.ModeHosted && oauthRedirectCallback(providerType) {
		callbackMode = "redirect"
		s.markOAuthPending(scope.AccountID(), id)
	}
	writeJSON(w, 200, map[string]any{"authorization_url": authURL, "redirect_uri": redirectURI, "callback_mode": callbackMode, "expires_in": int((10 * time.Minute) / time.Second)})
}

// persistOAuthToken writes a completed token with the flow's generation guard so
// a disconnect racing the completion cannot resurrect the connection. It is
// shared by the paste-back and redirect-callback completion paths.
func (s *Server) persistOAuthToken(ctx context.Context, accountID string, flow oauth.Flow, record oauth.TokenRecord) error {
	record.Generation = flow.Generation
	return s.scopeFor(accountID).PutOAuthTokenIfGeneration(ctx, oauth.TokenToStore(record), flow.Generation)
}

func (s *Server) completeProviderOAuth(w http.ResponseWriter, r *http.Request) {
	if s.oauthRateLimited(w, r, s.oauthCallbackLimiter) {
		return
	}
	id := r.PathValue("id")
	defer s.clearOAuthPending(s.scope(r).AccountID(), id)
	providerType, err := s.scope(r).ProviderType(r.Context(), id)
	if errors.Is(err, store.ErrProviderNotFound) {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	}
	if err != nil {
		adminError(w, 500, "database_error", "Could not load provider.")
		return
	}
	if providerType != "claude-subscription" {
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
	callback, err := claude.ParseCallback(input.RedirectedURL)
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
	tokens, err := claude.Exchange(r.Context(), s.providers.Registry().HTTPClient(), callback.Code, flow.RedirectURI, flow.PKCE.Verifier, flow.PKCE.State)
	if err != nil {
		adminError(w, 502, "oauth_exchange_failed", "OAuth token exchange failed.")
		return
	}
	record, err := oauth.MergeToken(oauth.TokenRecord{ProviderID: id}, tokens, time.Now().UTC())
	if err != nil {
		adminError(w, 502, "oauth_exchange_failed", "OAuth token exchange returned an invalid token.")
		return
	}
	if err := s.persistOAuthToken(r.Context(), s.scope(r).AccountID(), flow, record); err != nil {
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

// completeProviderOAuthRedirect handles the provider's browser redirect to
// /auth/callback. It is unauthenticated by necessity: the returning navigation
// is cross-site, so the SameSite=Strict user session cookie is not sent. The
// single-use OAuth state is the only correlation key, and it resolves to a
// server-side flow that already binds the account and provider — the browser
// never supplies either. This mirrors the Google sign-in callback.
func (s *Server) completeProviderOAuthRedirect(w http.ResponseWriter, r *http.Request) {
	key := clientIP(r, s.config.TrustedProxy)
	if !s.oauthCallbackLimiter.allowAttempt(key) {
		s.writeOAuthCallbackPage(w, http.StatusTooManyRequests, "Too many attempts", "Too many OAuth attempts. Return to Tiller and try again later.")
		return
	}
	query := r.URL.Query()
	state := strings.TrimSpace(query.Get("state"))
	providerError := strings.TrimSpace(query.Get("error"))
	if state == "" && providerError == "" {
		// No observable authorization code. A provider that returns the code for
		// manual copy (or a stray visit) lands here. The flow is untouched, so
		// guide the user back to the paste-back dialog.
		s.writeOAuthCallbackPage(w, http.StatusOK, "Finish connecting Tiller", "If you are connecting a provider to Tiller, copy this page's full URL from your browser address bar and paste it into the Tiller connection dialog.")
		return
	}
	flow, err := s.oauthFlows.TakeByState(state)
	if err != nil {
		s.recordOAuthFailure(r, s.oauthCallbackLimiter)
		s.writeOAuthCallbackPage(w, http.StatusBadRequest, "Connection expired", "This sign-in link is invalid, expired, or already used. Return to Tiller and start again.")
		return
	}
	defer s.clearOAuthPending(flow.AccountID, flow.ProviderID)
	if providerError != "" {
		s.writeOAuthCallbackPage(w, http.StatusOK, "Connection cancelled", "The provider declined the sign-in. Return to Tiller and try again.")
		return
	}
	providerType, err := s.scopeFor(flow.AccountID).ProviderType(r.Context(), flow.ProviderID)
	if err != nil {
		s.writeOAuthCallbackPage(w, http.StatusBadGateway, "Connection failed", "Could not load the provider. Return to Tiller and try again.")
		return
	}
	if providerType != "codex-subscription" && providerType != "claude-subscription" {
		s.writeOAuthCallbackPage(w, http.StatusBadRequest, "Connection failed", "OAuth is not supported for this provider.")
		return
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		s.writeOAuthCallbackPage(w, http.StatusBadRequest, "Connection failed", "The provider did not return an authorization code. Return to Tiller and try again.")
		return
	}
	var tokens oauth.TokenResponse
	if providerType == "codex-subscription" {
		tokens, err = codex.Exchange(r.Context(), s.providers.Registry().HTTPClient(), code, flow.RedirectURI, flow.PKCE.Verifier)
	} else {
		tokens, err = claude.Exchange(r.Context(), s.providers.Registry().HTTPClient(), code, flow.RedirectURI, flow.PKCE.Verifier, flow.PKCE.State)
	}
	if err != nil {
		s.writeOAuthCallbackPage(w, http.StatusBadGateway, "Connection failed", "OAuth token exchange failed. Return to Tiller and start again.")
		return
	}
	record, err := oauth.MergeToken(oauth.TokenRecord{ProviderID: flow.ProviderID}, tokens, time.Now().UTC())
	if err != nil {
		s.writeOAuthCallbackPage(w, http.StatusBadGateway, "Connection failed", "The provider returned an invalid token. Return to Tiller and start again.")
		return
	}
	if err := s.persistOAuthToken(r.Context(), flow.AccountID, flow, record); err != nil {
		if errors.Is(err, store.ErrOAuthGenerationChanged) {
			s.writeOAuthCallbackPage(w, http.StatusConflict, "Connection removed", "This provider was disconnected during sign-in. Return to Tiller and start again.")
			return
		}
		s.writeOAuthCallbackPage(w, http.StatusInternalServerError, "Connection failed", "Could not save the OAuth connection. Return to Tiller and try again.")
		return
	}
	s.oauthCallbackLimiter.success(key)
	s.writeOAuthCallbackPage(w, http.StatusOK, "Connected", "The provider is connected to Tiller. You can close this window and return to Tiller.")
}

// writeOAuthCallbackPage renders a minimal, self-contained HTML result page for
// the OAuth redirect callback. It carries no script and no inline style (the CSP
// is script-src/style-src 'self') and reflects no request input, so there is
// nothing to execute or leak.
func (s *Server) writeOAuthCallbackPage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>"+html.EscapeString(title)+" - Tiller</title></head><body><main><h1>"+html.EscapeString(title)+"</h1><p>"+html.EscapeString(message)+"</p></main></body></html>")
}

// beginDeviceFlow reuses a pending device authorization for the provider or
// registers a fresh one. When reused is true the caller should return the
// existing prompt without starting another poll.
func (s *Server) beginDeviceFlow(ctx context.Context, accountID, id string) (flowCtx context.Context, cancel context.CancelFunc, generation int64, prompt devicePrompt, reused bool, err error) {
	generation, err = s.scopeFor(accountID).OAuthGeneration(ctx, id)
	if err != nil {
		return nil, nil, 0, devicePrompt{}, false, err
	}
	s.oauthDeviceMu.Lock()
	if existing := s.oauthDevices[tenantKey(accountID, id)]; existing != nil && existing.Status == "pending" {
		prompt = existing.Prompt
		s.oauthDeviceMu.Unlock()
		return nil, nil, generation, prompt, true, nil
	}
	flowCtx, cancel = context.WithCancel(s.backgroundCtx)
	s.oauthDevices[tenantKey(accountID, id)] = &oauthDeviceState{Status: "pending", Generation: generation, Cancel: cancel}
	s.oauthDeviceMu.Unlock()
	currentGeneration, err := s.scopeFor(accountID).OAuthGeneration(ctx, id)
	if err != nil {
		s.clearDeviceState(accountID, id, cancel)
		return nil, nil, 0, devicePrompt{}, false, err
	}
	if currentGeneration != generation {
		s.clearDeviceState(accountID, id, cancel)
		return nil, nil, 0, devicePrompt{}, false, store.ErrOAuthGenerationChanged
	}
	return flowCtx, cancel, generation, devicePrompt{}, false, nil
}

func (s *Server) clearDeviceState(accountID, id string, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	s.oauthDeviceMu.Lock()
	delete(s.oauthDevices, tenantKey(accountID, id))
	s.oauthDeviceMu.Unlock()
}

func (s *Server) setDevicePrompt(accountID, id string, generation int64, prompt devicePrompt) {
	s.oauthDeviceMu.Lock()
	if state := s.oauthDevices[tenantKey(accountID, id)]; state != nil && state.Generation == generation {
		state.Prompt = prompt
	}
	s.oauthDeviceMu.Unlock()
}

func (s *Server) setDeviceConnected(accountID, id string, generation int64, record oauth.TokenRecord) {
	s.oauthDeviceMu.Lock()
	if state := s.oauthDevices[tenantKey(accountID, id)]; state != nil && state.Generation == generation {
		state.Status, state.Token = "connected", record
	}
	s.oauthDeviceMu.Unlock()
}

func (s *Server) storeDeviceToken(ctx context.Context, accountID, id string, generation int64, tokens oauth.TokenResponse) {
	record, err := oauth.MergeToken(oauth.TokenRecord{ProviderID: id}, tokens, time.Now().UTC())
	if err != nil {
		s.finishDevice(accountID, id, generation, "failed", err)
		return
	}
	record.Generation = generation
	if err := s.scopeFor(accountID).PutOAuthTokenIfGeneration(ctx, oauth.TokenToStore(record), generation); err != nil {
		s.finishDevice(accountID, id, generation, "failed", err)
		return
	}
	s.setDeviceConnected(accountID, id, generation, record)
}

func (s *Server) startGitHubDeviceFlow(ctx context.Context, accountID, id string) (devicePrompt, error) {
	flowCtx, _, generation, prompt, reused, err := s.beginDeviceFlow(ctx, accountID, id)
	if err != nil || reused {
		return prompt, err
	}
	device, err := github.RequestDeviceCode(flowCtx, s.providers.Registry().HTTPClient())
	if err != nil {
		s.finishDevice(accountID, id, generation, "failed", err)
		return devicePrompt{}, err
	}
	prompt = devicePrompt{UserCode: device.UserCode, VerificationURI: device.VerificationURI, VerificationURIComplete: device.VerificationURIComplete, ExpiresIn: device.ExpiresIn, Interval: device.Interval}
	s.setDevicePrompt(accountID, id, generation, prompt)
	go func() {
		tokens, err := github.PollToken(flowCtx, s.providers.Registry().HTTPClient(), device)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				s.finishDevice(accountID, id, generation, "failed", err)
			}
			return
		}
		user, _ := github.FetchUser(flowCtx, s.providers.Registry().HTTPClient(), tokens.AccessToken)
		copilot, _, err := github.FetchCopilotToken(flowCtx, s.providers.Registry().HTTPClient(), tokens.AccessToken)
		if err != nil {
			s.finishDevice(accountID, id, generation, "failed", err)
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
		s.storeDeviceToken(flowCtx, accountID, id, generation, tokens)
	}()
	return prompt, nil
}

// startCodexDeviceFlow signs in to Codex with the OpenAI device authorization
// flow. It is the hosted-safe equivalent of the Codex CLI's
// `codex login --device-auth`: the token exchange uses OpenAI's own registered
// device callback, so no client-supplied redirect URI is involved.
func (s *Server) startCodexDeviceFlow(ctx context.Context, accountID, id string) (devicePrompt, error) {
	flowCtx, _, generation, prompt, reused, err := s.beginDeviceFlow(ctx, accountID, id)
	if err != nil || reused {
		return prompt, err
	}
	device, err := codex.RequestDeviceCode(flowCtx, s.providers.Registry().HTTPClient())
	if err != nil {
		s.finishDevice(accountID, id, generation, "failed", err)
		return devicePrompt{}, err
	}
	prompt = devicePrompt{UserCode: device.UserCode, VerificationURI: device.VerificationURI, ExpiresIn: device.ExpiresIn, Interval: device.Interval}
	s.setDevicePrompt(accountID, id, generation, prompt)
	go func() {
		tokens, err := codex.PollDeviceToken(flowCtx, s.providers.Registry().HTTPClient(), device)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				s.finishDevice(accountID, id, generation, "failed", err)
			}
			return
		}
		s.storeDeviceToken(flowCtx, accountID, id, generation, tokens)
	}()
	return prompt, nil
}

func (s *Server) finishDevice(accountID, id string, generation int64, status string, err error) {
	s.oauthDeviceMu.Lock()
	defer s.oauthDeviceMu.Unlock()
	if state := s.oauthDevices[tenantKey(accountID, id)]; state != nil && state.Generation == generation {
		state.Status = status
		if err != nil {
			state.Err = "OAuth connection failed."
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
		if state.Prompt.VerificationURI != "" {
			result["verification_uri"] = state.Prompt.VerificationURI
			result["verification_uri_complete"] = state.Prompt.VerificationURIComplete
			result["user_code"] = state.Prompt.UserCode
			result["expires_in"] = state.Prompt.ExpiresIn
		}
		if state.Err != "" {
			result["error"] = state.Err
		}
		writeJSON(w, 200, result)
		return
	}
	if s.oauthPendingActive(s.scope(r).AccountID(), id) {
		writeJSON(w, 200, map[string]any{"status": "pending"})
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
	delete(s.oauthPending, tenantKey(scope.AccountID(), id))
	s.oauthDeviceMu.Unlock()
	s.oauthFlows.Cancel(s.scope(r).AccountID(), id)
	writeJSON(w, 200, map[string]any{"status": "disconnected"})
}
