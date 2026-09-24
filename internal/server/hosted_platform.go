package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/mailer"
	"github.com/tiller-router/tiller-router/internal/mailoutbox"
	"github.com/tiller-router/tiller-router/internal/store"
)

func (s *Server) platformLogin(w http.ResponseWriter, r *http.Request) {
	key := clientIP(r, s.config.TrustedProxy)
	if s.loginLimiter.locked(key) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many failed login attempts. Try again later.")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSONLimit(w, r, &input, authRequestMaxBytes); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.identity.AuthenticatePlatform(input.Username, input.Password, s.config.TillerPlatformAdminUser, s.config.TillerPlatformAdminPassword) {
		s.loginLimiter.recordFailure(key)
		adminError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid platform credentials.")
		return
	}
	s.loginLimiter.success(key)
	session, err := s.identity.CreatePlatformSession(r.Context())
	if err != nil {
		adminError(w, http.StatusInternalServerError, "internal_error", "Could not create session.")
		return
	}
	s.setPlatformSessionCookie(w, session.Token, session.ExpiresAt)
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.login", ActorType: "platform"})
	writeJSON(w, http.StatusOK, platformSessionPayload(session))
}

func (s *Server) requirePlatform(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(platformSessionCookie)
		if err != nil {
			adminError(w, http.StatusUnauthorized, "unauthorized", "Platform authentication required.")
			return
		}
		session, ok := s.identity.GetPlatformSession(r.Context(), cookie.Value)
		if !ok {
			adminError(w, http.StatusUnauthorized, "unauthorized", "Platform authentication required.")
			return
		}
		s.setPlatformSessionCookie(w, cookie.Value, session.ExpiresAt)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.identity.CheckPlatformCSRF(session, r.Header.Get("X-CSRF-Token")) {
			adminError(w, http.StatusForbidden, "csrf_failed", "A valid CSRF token is required.")
			return
		}
		ctx := contextWithPlatformSession(r.Context(), session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func contextWithPlatformSession(ctx context.Context, session identity.PlatformSession) context.Context {
	return context.WithValue(ctx, platformSessionKey, session)
}

func (s *Server) platformSessionStatus(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(platformSessionKey).(identity.PlatformSession)
	writeJSON(w, http.StatusOK, platformSessionPayload(session))
}

func (s *Server) platformLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(platformSessionCookie); err == nil {
		s.identity.DeletePlatformSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: platformSessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.logout", ActorType: "platform"})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setPlatformSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: platformSessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: maxAge(expires)})
}

func platformSessionPayload(session identity.PlatformSession) map[string]any {
	return map[string]any{"authenticated": true, "csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt.UTC()}
}

func (s *Server) platformSettings(w http.ResponseWriter, r *http.Request) {
	signup, err := s.storeHandle().HostedSignupEnabled(r.Context())
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load platform settings.")
		return
	}
	retention, err := s.storeHandle().AuditRetentionDays(r.Context())
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load platform settings.")
		return
	}
	status := s.mailer.Status()
	publicURL, _ := url.Parse(s.config.PublicURL)
	authSettings, err := s.storeHandle().GetPlatformAuthSettings(r.Context())
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "settings_locked", "Authentication settings are unavailable.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hosted_signup_enabled": signup, "audit_retention_days": retention, "mail": status,
		"google":    map[string]any{"enabled": authSettings.GoogleEnabled, "client_id": authSettings.GoogleClientID, "secret_configured": authSettings.GoogleClientSecret != "", "redirect_uri": strings.TrimRight(s.config.PublicURL, "/") + "/api/auth/google/callback"},
		"turnstile": map[string]any{"enabled": authSettings.TurnstileEnabled, "site_key": authSettings.TurnstileSiteKey, "secret_configured": authSettings.TurnstileSecret != "", "hostname": publicURL.Hostname()},
	})
}

func (s *Server) updatePlatformSettings(w http.ResponseWriter, r *http.Request) {
	var input struct {
		HostedSignupEnabled  *bool   `json:"hosted_signup_enabled"`
		AuditRetentionDays   *int    `json:"audit_retention_days"`
		MailProvider         *string `json:"mail_provider"`
		MailFrom             *string `json:"mail_from"`
		MailResendAPIKey     *string `json:"mail_resend_api_key"`
		MailBrevoAPIKey      *string `json:"mail_brevo_api_key"`
		MailSMTPHost         *string `json:"mail_smtp_host"`
		MailSMTPPort         *int    `json:"mail_smtp_port"`
		MailSMTPUsername     *string `json:"mail_smtp_username"`
		MailSMTPPassword     *string `json:"mail_smtp_password"`
		MailSMTPMode         *string `json:"mail_smtp_mode"`
		GoogleEnabled        *bool   `json:"google_signin_enabled"`
		GoogleClientID       *string `json:"google_client_id"`
		GoogleClientSecret   *string `json:"google_client_secret"`
		ClearGoogleSecret    bool    `json:"clear_google_client_secret"`
		TurnstileEnabled     *bool   `json:"turnstile_enabled"`
		TurnstileSiteKey     *string `json:"turnstile_site_key"`
		TurnstileSecret      *string `json:"turnstile_secret"`
		ClearTurnstileSecret bool    `json:"clear_turnstile_secret"`
	}
	if err := decodeJSONLimit(w, r, &input, 64<<10); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if input.GoogleClientID != nil && len(*input.GoogleClientID) > 2048 || input.GoogleClientSecret != nil && len(*input.GoogleClientSecret) > 8192 || input.TurnstileSiteKey != nil && len(*input.TurnstileSiteKey) > 2048 || input.TurnstileSecret != nil && len(*input.TurnstileSecret) > 8192 {
		adminError(w, http.StatusBadRequest, "invalid_auth_settings", "Authentication settings are too long.")
		return
	}
	s.platformSettingsMu.Lock()
	defer s.platformSettingsMu.Unlock()
	st := s.storeHandle()
	currentMail, err := st.GetPlatformMailSettings(r.Context())
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		adminError(w, http.StatusServiceUnavailable, "mail_locked", "Mail settings are unavailable.")
		return
	}
	if input.MailProvider != nil {
		currentMail.Provider = strings.ToLower(strings.TrimSpace(*input.MailProvider))
	}
	if input.MailFrom != nil {
		currentMail.From = strings.TrimSpace(*input.MailFrom)
	}
	if input.MailResendAPIKey != nil {
		currentMail.ResendAPIKey = *input.MailResendAPIKey
	}
	if input.MailBrevoAPIKey != nil {
		currentMail.BrevoAPIKey = *input.MailBrevoAPIKey
	}
	if input.MailSMTPHost != nil {
		currentMail.SMTPHost = strings.TrimSpace(*input.MailSMTPHost)
	}
	if input.MailSMTPPort != nil {
		currentMail.SMTPPort = strconv.Itoa(*input.MailSMTPPort)
	}
	if input.MailSMTPUsername != nil {
		currentMail.SMTPUsername = *input.MailSMTPUsername
	}
	if input.MailSMTPPassword != nil {
		currentMail.SMTPPassword = *input.MailSMTPPassword
	}
	if input.MailSMTPMode != nil {
		currentMail.SMTPMode = strings.ToLower(strings.TrimSpace(*input.MailSMTPMode))
	}
	authSettings, err := st.GetPlatformAuthSettings(r.Context())
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "settings_locked", "Authentication settings are unavailable.")
		return
	}
	wasGoogleEnabled := authSettings.GoogleEnabled
	previousGoogleClientID := authSettings.GoogleClientID
	if input.GoogleEnabled != nil {
		authSettings.GoogleEnabled = *input.GoogleEnabled
	}
	if input.GoogleClientID != nil {
		authSettings.GoogleClientID = strings.TrimSpace(*input.GoogleClientID)
	}
	if input.GoogleClientSecret != nil && *input.GoogleClientSecret != "" {
		authSettings.GoogleClientSecret = *input.GoogleClientSecret
	}
	if input.ClearGoogleSecret {
		authSettings.GoogleClientSecret = ""
	}
	if input.TurnstileEnabled != nil {
		authSettings.TurnstileEnabled = *input.TurnstileEnabled
	}
	if input.TurnstileSiteKey != nil {
		authSettings.TurnstileSiteKey = strings.TrimSpace(*input.TurnstileSiteKey)
	}
	if input.TurnstileSecret != nil && *input.TurnstileSecret != "" {
		authSettings.TurnstileSecret = *input.TurnstileSecret
	}
	if input.ClearTurnstileSecret {
		authSettings.TurnstileSecret = ""
	}
	googleDisableRequested := input.GoogleEnabled != nil && !*input.GoogleEnabled && wasGoogleEnabled
	googleClientIDChanged := input.GoogleClientID != nil && authSettings.GoogleClientID != previousGoogleClientID
	if googleDisableRequested || googleClientIDChanged {
		linked, countErr := s.identity.GoogleIdentityCount(r.Context())
		if countErr != nil {
			adminError(w, http.StatusInternalServerError, "database_error", "Could not check Google sign-in accounts.")
			return
		}
		if linked > 0 {
			adminError(w, http.StatusConflict, "google_accounts_linked", "Google sign-in cannot be disabled or moved to a different OAuth client while accounts are linked. Have each user add a password and unlink Google first.")
			return
		}
	}
	if authSettings.GoogleEnabled && (authSettings.GoogleClientID == "" || authSettings.GoogleClientSecret == "") {
		adminError(w, http.StatusBadRequest, "invalid_google_settings", "Google sign-in needs a client ID and client secret.")
		return
	}
	if authSettings.TurnstileEnabled && (authSettings.TurnstileSiteKey == "" || authSettings.TurnstileSecret == "") {
		adminError(w, http.StatusBadRequest, "invalid_turnstile_settings", "Turnstile needs a site key and secret key.")
		return
	}
	retention, err := st.AuditRetentionDays(r.Context())
	if errors.Is(err, sql.ErrNoRows) {
		retention = 30
	} else if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load platform settings.")
		return
	}
	if input.AuditRetentionDays != nil {
		retention = *input.AuditRetentionDays
	}
	signup, err := st.HostedSignupEnabled(r.Context())
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load platform settings.")
		return
	}
	if input.HostedSignupEnabled != nil {
		signup = *input.HostedSignupEnabled
	}
	cfg := mailer.Config{Provider: currentMail.Provider, From: currentMail.From, ResendAPIKey: currentMail.ResendAPIKey, BrevoAPIKey: currentMail.BrevoAPIKey, SMTPHost: currentMail.SMTPHost, SMTPPort: parseMailPort(currentMail.SMTPPort), SMTPUsername: currentMail.SMTPUsername, SMTPPassword: currentMail.SMTPPassword, SMTPMode: currentMail.SMTPMode}
	if err := mailer.Validate(cfg); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_mail_settings", "Mail settings are invalid.")
		return
	}
	if err := st.SavePlatformSettings(r.Context(), store.PlatformSettingsProposal{HostedSignupEnabled: signup, AuditRetentionDays: retention, Mail: currentMail, Auth: authSettings}); err != nil {
		adminError(w, http.StatusServiceUnavailable, "settings_locked", "Platform settings could not be saved.")
		return
	}
	if cfg.Provider == "" {
		s.mailer.Clear()
	} else if err := s.mailer.Update(cfg); err != nil {
		adminError(w, http.StatusInternalServerError, "internal_error", "Mail settings could not be activated.")
		return
	}
	if input.HostedSignupEnabled != nil {
		s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.signup_setting_changed", ActorType: "platform", Metadata: map[string]string{"enabled": strconv.FormatBool(*input.HostedSignupEnabled)}})
	}
	if hasMailUpdate(input) {
		s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.mail_settings_changed", ActorType: "platform"})
	}
	if input.GoogleEnabled != nil || input.GoogleClientID != nil || input.GoogleClientSecret != nil || input.ClearGoogleSecret {
		s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.google_signin_settings_changed", ActorType: "platform", Metadata: map[string]string{"enabled": strconv.FormatBool(authSettings.GoogleEnabled)}})
	}
	if input.TurnstileEnabled != nil || input.TurnstileSiteKey != nil || input.TurnstileSecret != nil || input.ClearTurnstileSecret {
		s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.turnstile_settings_changed", ActorType: "platform", Metadata: map[string]string{"enabled": strconv.FormatBool(authSettings.TurnstileEnabled)}})
	}
	w.WriteHeader(http.StatusNoContent)
}

func hasMailUpdate(input struct {
	HostedSignupEnabled  *bool   `json:"hosted_signup_enabled"`
	AuditRetentionDays   *int    `json:"audit_retention_days"`
	MailProvider         *string `json:"mail_provider"`
	MailFrom             *string `json:"mail_from"`
	MailResendAPIKey     *string `json:"mail_resend_api_key"`
	MailBrevoAPIKey      *string `json:"mail_brevo_api_key"`
	MailSMTPHost         *string `json:"mail_smtp_host"`
	MailSMTPPort         *int    `json:"mail_smtp_port"`
	MailSMTPUsername     *string `json:"mail_smtp_username"`
	MailSMTPPassword     *string `json:"mail_smtp_password"`
	MailSMTPMode         *string `json:"mail_smtp_mode"`
	GoogleEnabled        *bool   `json:"google_signin_enabled"`
	GoogleClientID       *string `json:"google_client_id"`
	GoogleClientSecret   *string `json:"google_client_secret"`
	ClearGoogleSecret    bool    `json:"clear_google_client_secret"`
	TurnstileEnabled     *bool   `json:"turnstile_enabled"`
	TurnstileSiteKey     *string `json:"turnstile_site_key"`
	TurnstileSecret      *string `json:"turnstile_secret"`
	ClearTurnstileSecret bool    `json:"clear_turnstile_secret"`
}) bool {
	return input.MailProvider != nil || input.MailFrom != nil || input.MailResendAPIKey != nil || input.MailBrevoAPIKey != nil || input.MailSMTPHost != nil || input.MailSMTPPort != nil || input.MailSMTPUsername != nil || input.MailSMTPPassword != nil || input.MailSMTPMode != nil
}

func (s *Server) platformUsers(w http.ResponseWriter, r *http.Request) {
	limit, offset, search := pagination(r)
	rows, err := s.identity.ListUsers(r.Context(), search, limit, offset)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not list users.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows, "limit": limit, "offset": offset})
}

func (s *Server) platformAudit(w http.ResponseWriter, r *http.Request) {
	limit, offset, _ := pagination(r)
	rows, err := s.storeHandle().ListPlatformAudit(r.Context(), limit, offset)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not list audit events.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows, "limit": limit, "offset": offset})
}

// platformMailQueue reports queued, sent, and recently dead-lettered mail plus a
// small recent delivery log for the operator dashboard. Dead is a recent-window
// count so an all-time total cannot masquerade as an active incident.
func (s *Server) platformMailQueue(w http.ResponseWriter, r *http.Request) {
	if s.outbox == nil {
		writeJSON(w, http.StatusOK, map[string]any{"queued": 0, "sent_recent": 0, "dead_recent": 0, "log": []mailoutbox.MailLogEntry{}})
		return
	}
	queued, dead, err := s.outbox.DueCounts(r.Context(), 24*time.Hour)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load the mail queue.")
		return
	}
	sent, err := s.outbox.SentCount(r.Context(), 24*time.Hour)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load the mail queue.")
		return
	}
	logRows, err := s.outbox.Recent(r.Context(), 25)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load the mail queue.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": queued, "sent_recent": sent, "dead_recent": dead, "log": logRows})
}

func (s *Server) accountAudit(w http.ResponseWriter, r *http.Request) {
	limit, offset, _ := pagination(r)
	rows, err := s.scope(r).ListAccountAudit(r.Context(), limit, offset)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not list audit events.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows, "limit": limit, "offset": offset})
}

func (s *Server) changeAccountStatus(w http.ResponseWriter, r *http.Request, status string) {
	accountID := r.PathValue("id")
	if err := s.identity.SetAccountStatus(r.Context(), accountID, status); err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			adminError(w, http.StatusNotFound, "not_found", "Account not found.")
		} else if errors.Is(err, identity.ErrAccountDeleting) {
			adminError(w, http.StatusConflict, "account_deleting", "Account deletion is already in progress.")
		} else {
			adminError(w, http.StatusInternalServerError, "database_error", "Could not change account status.")
		}
		return
	}
	s.clients.InvalidateAccount(accountID)
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.account_" + status, ActorType: "platform", TargetType: "account", TargetID: accountID})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) suspendAccount(w http.ResponseWriter, r *http.Request) {
	s.changeAccountStatus(w, r, "suspended")
}

func (s *Server) unsuspendAccount(w http.ResponseWriter, r *http.Request) {
	s.changeAccountStatus(w, r, "active")
}

func (s *Server) revokeAccountSessions(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	s.identity.InvalidateAccount(accountID)
	s.clients.InvalidateAccount(accountID)
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.account_sessions_revoked", ActorType: "platform", TargetType: "account", TargetID: accountID})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	var input struct {
		Confirm string `json:"confirm"`
	}
	if err := decodeJSON(w, r, &input); err != nil || strings.TrimSpace(input.Confirm) != accountID {
		adminError(w, http.StatusBadRequest, "confirmation_required", "Type the account ID to confirm deletion.")
		return
	}
	if err := s.purgeAccount(r.Context(), accountID); err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			adminError(w, http.StatusNotFound, "not_found", "Account not found.")
		} else {
			adminError(w, http.StatusInternalServerError, "delete_failed", "Could not delete account.")
		}
		return
	}
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.account_deleted", ActorType: "platform", TargetType: "account", TargetID: accountID})
	w.WriteHeader(http.StatusNoContent)
}
