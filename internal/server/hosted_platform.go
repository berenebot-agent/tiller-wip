package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/mailer"
	"github.com/tiller-router/tiller-router/internal/store"
)

func (s *Server) platformLogin(w http.ResponseWriter, r *http.Request) {
	key := peerIP(r)
	if s.loginLimiter.locked(key) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many failed login attempts. Try again later.")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.identity.AuthenticatePlatform(input.Username, input.Password, s.config.AdminUsername, s.config.AdminPassword) {
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
	writeJSON(w, http.StatusOK, map[string]any{"hosted_signup_enabled": signup, "audit_retention_days": retention, "mail": status})
}

func (s *Server) updatePlatformSettings(w http.ResponseWriter, r *http.Request) {
	var input struct {
		HostedSignupEnabled *bool   `json:"hosted_signup_enabled"`
		AuditRetentionDays  *int    `json:"audit_retention_days"`
		MailProvider        *string `json:"mail_provider"`
		MailFrom            *string `json:"mail_from"`
		MailResendAPIKey    *string `json:"mail_resend_api_key"`
		MailSMTPHost        *string `json:"mail_smtp_host"`
		MailSMTPPort        *int    `json:"mail_smtp_port"`
		MailSMTPUsername    *string `json:"mail_smtp_username"`
		MailSMTPPassword    *string `json:"mail_smtp_password"`
		MailSMTPMode        *string `json:"mail_smtp_mode"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	st := s.storeHandle()
	if input.HostedSignupEnabled != nil {
		if err := st.SetHostedSignupEnabled(r.Context(), *input.HostedSignupEnabled); err != nil {
			adminError(w, http.StatusInternalServerError, "database_error", "Could not update signup settings.")
			return
		}
		s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.signup_setting_changed", ActorType: "platform", Metadata: map[string]string{"enabled": strconv.FormatBool(*input.HostedSignupEnabled)}})
	}
	if input.AuditRetentionDays != nil {
		if err := st.SetAuditRetentionDays(r.Context(), *input.AuditRetentionDays); err != nil {
			adminError(w, http.StatusBadRequest, "invalid_audit_retention", "Audit retention must be at least one day.")
			return
		}
	}
	if hasMailUpdate(input) {
		current, err := st.GetPlatformMailSettings(r.Context())
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "mail_locked", "Mail settings are unavailable.")
			return
		}
		if input.MailProvider != nil {
			current.Provider = strings.ToLower(strings.TrimSpace(*input.MailProvider))
		}
		if input.MailFrom != nil {
			current.From = strings.TrimSpace(*input.MailFrom)
		}
		if input.MailResendAPIKey != nil {
			current.ResendAPIKey = *input.MailResendAPIKey
		}
		if input.MailSMTPHost != nil {
			current.SMTPHost = strings.TrimSpace(*input.MailSMTPHost)
		}
		if input.MailSMTPPort != nil {
			current.SMTPPort = strconv.Itoa(*input.MailSMTPPort)
		}
		if input.MailSMTPUsername != nil {
			current.SMTPUsername = *input.MailSMTPUsername
		}
		if input.MailSMTPPassword != nil {
			current.SMTPPassword = *input.MailSMTPPassword
		}
		if input.MailSMTPMode != nil {
			current.SMTPMode = strings.ToLower(strings.TrimSpace(*input.MailSMTPMode))
		}
		cfg := mailer.Config{Provider: current.Provider, From: current.From, ResendAPIKey: current.ResendAPIKey, SMTPHost: current.SMTPHost, SMTPPort: parseMailPort(current.SMTPPort), SMTPUsername: current.SMTPUsername, SMTPPassword: current.SMTPPassword, SMTPMode: current.SMTPMode}
		if cfg.Provider == "" {
			s.mailer.Clear()
		} else if err := s.mailer.Update(cfg); err != nil {
			adminError(w, http.StatusBadRequest, "invalid_mail_settings", "Mail settings are invalid.")
			return
		}
		values := map[string]string{store.PlatformSettingMailProvider: current.Provider, store.PlatformSettingMailFrom: current.From, store.PlatformSettingMailResendAPIKey: current.ResendAPIKey, store.PlatformSettingMailSMTPHost: current.SMTPHost, store.PlatformSettingMailSMTPPort: current.SMTPPort, store.PlatformSettingMailSMTPUsername: current.SMTPUsername, store.PlatformSettingMailSMTPPassword: current.SMTPPassword, store.PlatformSettingMailSMTPMode: current.SMTPMode}
		for key, value := range values {
			if err := st.SetPlatformSetting(r.Context(), key, value); err != nil {
				adminError(w, http.StatusServiceUnavailable, "mail_settings_locked", "Mail secrets could not be saved.")
				return
			}
		}
		s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.mail_settings_changed", ActorType: "platform"})
	}
	w.WriteHeader(http.StatusNoContent)
}

func hasMailUpdate(input struct {
	HostedSignupEnabled *bool   `json:"hosted_signup_enabled"`
	AuditRetentionDays  *int    `json:"audit_retention_days"`
	MailProvider        *string `json:"mail_provider"`
	MailFrom            *string `json:"mail_from"`
	MailResendAPIKey    *string `json:"mail_resend_api_key"`
	MailSMTPHost        *string `json:"mail_smtp_host"`
	MailSMTPPort        *int    `json:"mail_smtp_port"`
	MailSMTPUsername    *string `json:"mail_smtp_username"`
	MailSMTPPassword    *string `json:"mail_smtp_password"`
	MailSMTPMode        *string `json:"mail_smtp_mode"`
}) bool {
	return input.MailProvider != nil || input.MailFrom != nil || input.MailResendAPIKey != nil || input.MailSMTPHost != nil || input.MailSMTPPort != nil || input.MailSMTPUsername != nil || input.MailSMTPPassword != nil || input.MailSMTPMode != nil
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
	if accountID == database.LocalAccountID {
		adminError(w, http.StatusBadRequest, "invalid_account", "The local account cannot be changed from hosted controls.")
		return
	}
	if err := s.identity.SetAccountStatus(r.Context(), accountID, status); err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			adminError(w, http.StatusNotFound, "not_found", "Account not found.")
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
	if accountID == database.LocalAccountID {
		adminError(w, http.StatusBadRequest, "invalid_account", "The local account cannot be deleted from hosted controls.")
		return
	}
	var input struct {
		Confirm string `json:"confirm"`
	}
	if err := decodeJSON(w, r, &input); err != nil || strings.TrimSpace(input.Confirm) != accountID {
		adminError(w, http.StatusBadRequest, "confirmation_required", "Type the account ID to confirm deletion.")
		return
	}
	if err := s.identity.SetAccountStatus(r.Context(), accountID, "deleting"); err != nil {
		adminError(w, http.StatusNotFound, "not_found", "Account not found.")
		return
	}
	s.identity.InvalidateAccount(accountID)
	s.clients.InvalidateAccount(accountID)
	if err := s.storeHandle().For(accountID).DeleteAccountResources(r.Context()); err != nil {
		adminError(w, http.StatusInternalServerError, "delete_failed", "Could not delete account resources.")
		return
	}
	if err := s.storeHandle().DeleteAccountActivity(accountID); err != nil {
		adminError(w, http.StatusInternalServerError, "delete_failed", "Could not delete account activity.")
		return
	}
	if err := s.identity.DeleteAccountIdentity(r.Context(), accountID); err != nil {
		adminError(w, http.StatusInternalServerError, "delete_failed", "Could not delete account identity.")
		return
	}
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.account_deleted", ActorType: "platform", TargetType: "account", TargetID: accountID})
	w.WriteHeader(http.StatusNoContent)
}
