package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/hostedauth"
	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/store"
)

const (
	googleStateCookie   = "__Host-tiller_google_state"
	googleSignupCookie  = "__Host-tiller_google_signup"
	googleReauthTTL     = 5 * time.Minute
	googleStateMaxBytes = 128
)

func (s *Server) googleConfig(ctx context.Context) (hostedauth.GoogleConfig, error) {
	settings, err := s.storeHandle().GetPlatformAuthSettings(ctx)
	if err != nil {
		return hostedauth.GoogleConfig{}, err
	}
	if !settings.GoogleEnabled || settings.GoogleClientID == "" || settings.GoogleClientSecret == "" {
		return hostedauth.GoogleConfig{}, errors.New("Google sign-in is not configured")
	}
	return hostedauth.GoogleConfig{
		ClientID: settings.GoogleClientID, ClientSecret: settings.GoogleClientSecret,
		RedirectURI: strings.TrimRight(s.config.PublicURL, "/") + "/api/auth/google/callback",
	}, nil
}

func (s *Server) startGoogleSignIn(w http.ResponseWriter, r *http.Request) {
	key := clientIP(r, s.config.TrustedProxy)
	if !s.oauthStartLimiter.allowAttempt(key) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many sign-in attempts. Try again later.")
		return
	}
	var input struct {
		CaptchaToken string `json:"captcha_token"`
	}
	if err := decodeJSONLimit(w, r, &input, authRequestMaxBytes); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.verifyAuthCaptcha(w, r, input.CaptchaToken, "google_signin") {
		return
	}
	if err := s.beginGoogleFlow(w, r, hostedauth.IntentSignIn, "", false); err != nil {
		adminError(w, http.StatusServiceUnavailable, "google_unavailable", "Google sign-in is temporarily unavailable.")
		return
	}
}

func (s *Server) startGoogleLink(w http.ResponseWriter, r *http.Request) {
	if !s.oauthStartLimiter.allowAttempt(clientIP(r, s.config.TrustedProxy)) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many sign-in attempts. Try again later.")
		return
	}
	session := r.Context().Value(userSessionKey).(identity.UserSession)
	var input struct {
		CurrentPassword string `json:"current_password"`
	}
	if err := decodeJSONLimit(w, r, &input, authRequestMaxBytes); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.identity.VerifyPassword(r.Context(), session.User.ID, input.CurrentPassword); err != nil {
		adminError(w, http.StatusUnauthorized, "invalid_credentials", "Your current password is incorrect.")
		return
	}
	if err := s.beginGoogleFlow(w, r, hostedauth.IntentLink, rawUserSessionToken(r), true); err != nil {
		adminError(w, http.StatusServiceUnavailable, "google_unavailable", "Google linking is temporarily unavailable.")
	}
}

func (s *Server) startGoogleReauth(w http.ResponseWriter, r *http.Request) {
	if !s.oauthStartLimiter.allowAttempt(clientIP(r, s.config.TrustedProxy)) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many sign-in attempts. Try again later.")
		return
	}
	session := r.Context().Value(userSessionKey).(identity.UserSession)
	profile, err := s.identity.AccountProfile(r.Context(), session.User.ID)
	if err != nil || !profile.GoogleLinked {
		adminError(w, http.StatusConflict, "google_not_linked", "Link Google to this account before using it to confirm account changes.")
		return
	}
	if err := s.beginGoogleFlow(w, r, hostedauth.IntentReauth, rawUserSessionToken(r), true); err != nil {
		adminError(w, http.StatusServiceUnavailable, "google_unavailable", "Google confirmation is temporarily unavailable.")
	}
}

func (s *Server) beginGoogleFlow(w http.ResponseWriter, r *http.Request, intent hostedauth.Intent, sessionToken string, selectAccount bool) error {
	config, err := s.googleConfig(r.Context())
	if err != nil {
		return err
	}
	verifier, err := hostedauth.NewPKCEVerifier()
	if err != nil {
		return err
	}
	state, err := newGoogleRandomValue()
	if err != nil {
		return err
	}
	nonce, err := newGoogleRandomValue()
	if err != nil {
		return err
	}
	flow := hostedauth.Flow{State: state, Nonce: nonce, Verifier: verifier, Intent: intent, SessionToken: sessionToken, ExpiresAt: time.Now().Add(hostedauth.FlowTTL)}
	if !s.googleFlows.Put(flow) {
		return errors.New("Google sign-in flow capacity reached")
	}
	redirect, err := hostedauth.AuthorizationURL(config, state, nonce, verifier, selectAccount)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: googleStateCookie, Value: state, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, Expires: flow.ExpiresAt, MaxAge: maxAge(flow.ExpiresAt)})
	writeJSON(w, http.StatusOK, map[string]string{"redirect_url": redirect})
	return nil
}

func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	if !s.oauthCallbackLimiter.allowAttempt(clientIP(r, s.config.TrustedProxy)) {
		s.googleCallbackError(w, r, "google_failed")
		return
	}
	stateCookie, err := r.Cookie(googleStateCookie)
	state := r.URL.Query().Get("state")
	if err != nil || len(state) > googleStateMaxBytes || subtle.ConstantTimeCompare([]byte(state), []byte(stateCookie.Value)) != 1 {
		s.googleCallbackError(w, r, "google_expired")
		return
	}
	flow, ok := s.googleFlows.Take(state)
	s.clearCookie(w, googleStateCookie)
	if !ok || r.URL.Query().Get("error") != "" {
		s.googleCallbackError(w, r, "google_failed")
		return
	}
	config, err := s.googleConfig(r.Context())
	if err != nil {
		s.googleCallbackError(w, r, "google_unavailable")
		return
	}
	claims, err := s.googleVerifier.ExchangeCode(r.Context(), config, r.URL.Query().Get("code"), flow.Verifier, flow.Nonce)
	if err != nil {
		s.googleCallbackError(w, r, "google_failed")
		return
	}
	s.platformSettingsMu.Lock()
	defer s.platformSettingsMu.Unlock()
	current, err := s.storeHandle().GetPlatformAuthSettings(r.Context())
	if err != nil || !current.GoogleEnabled || current.GoogleClientID != config.ClientID || current.GoogleClientSecret != config.ClientSecret {
		s.googleCallbackError(w, r, "google_unavailable")
		return
	}
	switch flow.Intent {
	case hostedauth.IntentSignIn:
		s.completeGoogleSignIn(w, r, claims)
	case hostedauth.IntentLink:
		s.completeGoogleLink(w, r, flow, claims)
	case hostedauth.IntentReauth:
		s.completeGoogleReauth(w, r, flow, claims)
	default:
		s.googleCallbackError(w, r, "google_failed")
	}
}

func (s *Server) completeGoogleSignIn(w http.ResponseWriter, r *http.Request, claims hostedauth.GoogleIdentity) {
	u, err := s.identity.GoogleUserBySubject(r.Context(), claims.Subject)
	if err == nil {
		session, sessionErr := s.identity.CreateUserSession(r.Context(), u)
		if sessionErr != nil {
			s.googleCallbackError(w, r, "google_failed")
			return
		}
		s.setUserSessionCookie(w, r, session.Token, session.ExpiresAt)
		s.recordAccountAudit(r.Context(), u.AccountID, store.AuditEvent{Event: "user.login", ActorType: "user", ActorID: u.ID, Metadata: map[string]string{"method": "google"}})
		http.Redirect(w, r, "/#clients", http.StatusSeeOther)
		return
	}
	if !errors.Is(err, identity.ErrNotFound) {
		s.googleCallbackError(w, r, "google_unavailable")
		return
	}
	if _, err := s.identity.UserByEmail(r.Context(), claims.Email); err == nil {
		s.googleCallbackError(w, r, "google_link_required")
		return
	} else if !errors.Is(err, identity.ErrNotFound) {
		s.googleCallbackError(w, r, "google_unavailable")
		return
	}
	enabled, err := s.storeHandle().HostedSignupEnabled(r.Context())
	if err != nil || !enabled {
		s.googleCallbackError(w, r, "signup_unavailable")
		return
	}
	pendingToken, ok := s.googlePending.Put(hostedauth.SignupClaims{Subject: claims.Subject, Email: claims.Email})
	if !ok {
		s.googleCallbackError(w, r, "google_unavailable")
		return
	}
	expires := time.Now().Add(hostedauth.FlowTTL)
	http.SetCookie(w, &http.Cookie{Name: googleSignupCookie, Value: pendingToken, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: maxAge(expires)})
	http.Redirect(w, r, "/login?google_signup=1", http.StatusSeeOther)
}

func (s *Server) completeGoogleLink(w http.ResponseWriter, r *http.Request, flow hostedauth.Flow, claims hostedauth.GoogleIdentity) {
	session, ok := s.identity.GetUserSession(r.Context(), flow.SessionToken)
	if !ok {
		s.googleCallbackError(w, r, "google_session_expired")
		return
	}
	if err := s.identity.LinkGoogleIdentity(r.Context(), session.User.ID, claims.Subject, claims.Email); err != nil {
		if errors.Is(err, identity.ErrGoogleIdentityTaken) {
			s.googleCallbackError(w, r, "google_already_linked")
		} else {
			s.googleCallbackError(w, r, "google_unavailable")
		}
		return
	}
	s.setUserSessionCookie(w, r, flow.SessionToken, session.ExpiresAt)
	s.recordAccountAudit(r.Context(), session.User.AccountID, store.AuditEvent{Event: "user.google_linked", ActorType: "user", ActorID: session.User.ID})
	http.Redirect(w, r, "/?google_linked=1#settings/account", http.StatusSeeOther)
}

func (s *Server) completeGoogleReauth(w http.ResponseWriter, r *http.Request, flow hostedauth.Flow, claims hostedauth.GoogleIdentity) {
	session, ok := s.identity.GetUserSession(r.Context(), flow.SessionToken)
	if !ok {
		s.googleCallbackError(w, r, "google_session_expired")
		return
	}
	u, err := s.identity.GoogleUserBySubject(r.Context(), claims.Subject)
	if err != nil || u.ID != session.User.ID {
		s.googleCallbackError(w, r, "google_failed")
		return
	}
	s.grantGoogleReauth(flow.SessionToken)
	s.setUserSessionCookie(w, r, flow.SessionToken, session.ExpiresAt)
	s.recordAccountAudit(r.Context(), session.User.AccountID, store.AuditEvent{Event: "user.google_reauthenticated", ActorType: "user", ActorID: session.User.ID})
	http.Redirect(w, r, "/?google_reauth=1#settings/account", http.StatusSeeOther)
}

func (s *Server) completeGoogleSignup(w http.ResponseWriter, r *http.Request) {
	key := clientIP(r, s.config.TrustedProxy)
	if !s.signupLimiter.allowAttempt(key) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many signup attempts. Try again later.")
		return
	}
	var input struct {
		AcceptTerms bool `json:"accept_terms"`
	}
	if err := decodeJSONLimit(w, r, &input, authRequestMaxBytes); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !input.AcceptTerms {
		adminError(w, http.StatusBadRequest, "terms_not_accepted", "You must accept the Terms of Service and Privacy Policy.")
		return
	}
	cookie, err := r.Cookie(googleSignupCookie)
	if err != nil {
		adminError(w, http.StatusBadRequest, "google_signup_expired", "Start Google sign-in again to continue.")
		return
	}
	claims, ok := s.googlePending.Take(cookie.Value)
	s.clearCookie(w, googleSignupCookie)
	if !ok {
		adminError(w, http.StatusBadRequest, "google_signup_expired", "Start Google sign-in again to continue.")
		return
	}
	s.platformSettingsMu.Lock()
	defer s.platformSettingsMu.Unlock()
	authSettings, err := s.storeHandle().GetPlatformAuthSettings(r.Context())
	if err != nil || !authSettings.GoogleEnabled || authSettings.GoogleClientID == "" || authSettings.GoogleClientSecret == "" {
		adminError(w, http.StatusServiceUnavailable, "google_unavailable", "Google sign-in is temporarily unavailable.")
		return
	}
	enabled, err := s.storeHandle().HostedSignupEnabled(r.Context())
	if err != nil || !enabled {
		adminError(w, http.StatusServiceUnavailable, "signup_unavailable", "Signup is currently unavailable.")
		return
	}
	terms, err := s.storeHandle().GetLegalDoc(r.Context(), "terms")
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "signup_unavailable", "Signup is currently unavailable.")
		return
	}
	privacy, err := s.storeHandle().GetLegalDoc(r.Context(), "privacy")
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "signup_unavailable", "Signup is currently unavailable.")
		return
	}
	acceptance := identity.SignupAcceptance{TermsUpdatedAt: terms.UpdatedAt, PrivacyUpdatedAt: privacy.UpdatedAt, IP: clientIP(r, s.config.TrustedProxy), UserAgent: r.UserAgent()}
	u, err := s.identity.CreateGoogleSignup(r.Context(), claims.Email, claims.Subject, acceptance)
	if err != nil {
		if _, lookupErr := s.identity.UserByEmail(r.Context(), claims.Email); lookupErr == nil || errors.Is(err, identity.ErrGoogleIdentityTaken) {
			adminError(w, http.StatusConflict, "google_link_required", "This Google email already has a Tiller account. Sign in with that account, then link Google in Account settings.")
			return
		}
		adminError(w, http.StatusServiceUnavailable, "signup_unavailable", "Signup is currently unavailable.")
		return
	}
	session, err := s.identity.CreateUserSession(r.Context(), u)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "session_failed", "Could not create a sign-in session.")
		return
	}
	s.setUserSessionCookie(w, r, session.Token, session.ExpiresAt)
	s.recordAccountAudit(r.Context(), u.AccountID, store.AuditEvent{Event: "user.signup", ActorType: "user", ActorID: u.ID, Metadata: map[string]string{"method": "google"}})
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "user.signup", ActorType: "user", TargetType: "user", TargetID: u.ID, Metadata: map[string]string{"method": "google"}})
	writeJSON(w, http.StatusOK, userSessionPayload(session))
}

func (s *Server) unlinkGoogle(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(userSessionKey).(identity.UserSession)
	var input struct {
		Password string `json:"password"`
	}
	if err := decodeJSONLimit(w, r, &input, authRequestMaxBytes); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if _, err := s.reauthenticateSensitive(r, session.User.ID, input.Password); err != nil {
		adminError(w, http.StatusUnauthorized, "reauth_required", "Confirm your identity with your password or Google before unlinking Google.")
		return
	}
	if err := s.identity.UnlinkGoogleIdentity(r.Context(), session.User.ID); err != nil {
		if errors.Is(err, identity.ErrLastIdentity) {
			adminError(w, http.StatusConflict, "last_signin_method", "Set a password before removing Google as your sign-in method.")
			return
		}
		adminError(w, http.StatusConflict, "google_unavailable", "Google could not be unlinked from this account.")
		return
	}
	s.recordAccountAudit(r.Context(), session.User.AccountID, store.AuditEvent{Event: "user.google_unlinked", ActorType: "user", ActorID: session.User.ID})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) googleCallbackError(w http.ResponseWriter, r *http.Request, code string) {
	s.clearCookie(w, googleStateCookie)
	target := "/login?auth_error=" + code
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) grantGoogleReauth(sessionToken string) {
	key := sha256.Sum256([]byte(sessionToken))
	now := time.Now()
	s.googleReauthMu.Lock()
	defer s.googleReauthMu.Unlock()
	for existing, expires := range s.googleReauth {
		if !now.Before(expires) {
			delete(s.googleReauth, existing)
		}
	}
	if len(s.googleReauth) >= 100000 {
		for existing := range s.googleReauth {
			delete(s.googleReauth, existing)
			break
		}
	}
	s.googleReauth[key] = now.Add(googleReauthTTL)
}

func (s *Server) consumeGoogleReauth(sessionToken string) bool {
	key := sha256.Sum256([]byte(sessionToken))
	now := time.Now()
	s.googleReauthMu.Lock()
	defer s.googleReauthMu.Unlock()
	expires, ok := s.googleReauth[key]
	delete(s.googleReauth, key)
	return ok && now.Before(expires)
}

func (s *Server) reauthenticateSensitive(r *http.Request, userID, password string) (bool, error) {
	err := s.identity.VerifyPassword(r.Context(), userID, password)
	if err == nil {
		s.consumeGoogleReauth(rawUserSessionToken(r))
		return false, nil
	}
	if s.consumeGoogleReauth(rawUserSessionToken(r)) {
		return true, nil
	}
	return false, err
}

func newGoogleRandomValue() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
