package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/mailer"
	"github.com/tiller-router/tiller-router/internal/store"
)

const (
	genericSignupMessage = "If the address can receive mail, a verification message will arrive shortly."
	genericResetMessage  = "If the address belongs to an account, a password-reset message will arrive shortly."
)

func platformMailSettings(m config.MailBootstrap) store.PlatformMailSettings {
	return store.PlatformMailSettings{
		Provider: m.Provider, From: m.From,
		SMTPHost: m.SMTPHost, SMTPPort: strconv.Itoa(m.SMTPPort),
		SMTPUsername: m.SMTPUsername, SMTPPassword: m.SMTPPassword, SMTPMode: m.SMTPMode,
	}
}

func parseMailPort(raw string) int {
	port, _ := strconv.Atoi(raw)
	return port
}

func (s *Server) runtime(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"mode": string(s.config.Mode)})
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	key := peerIP(r)
	if s.signupLimiter.locked(key) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many signup attempts. Try again later.")
		return
	}
	enabled, err := s.storeHandle().HostedSignupEnabled(r.Context())
	if err != nil || !enabled {
		adminError(w, http.StatusServiceUnavailable, "signup_unavailable", "Signup is currently unavailable.")
		return
	}
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !validEmail(input.Email) {
		adminError(w, http.StatusBadRequest, "invalid_request", "Enter a valid email address.")
		return
	}
	result, err := s.identity.CreateSignup(r.Context(), input.Email, input.Password)
	if err != nil {
		if errors.Is(err, identity.ErrWeakPassword) || errors.Is(err, identity.ErrPasswordTooLong) {
			adminError(w, http.StatusBadRequest, "invalid_password", "Password must be between 12 and 1024 bytes.")
			return
		}
		s.signupLimiter.recordFailure(key)
		writeJSON(w, http.StatusAccepted, map[string]any{"message": genericSignupMessage})
		return
	}
	s.signupLimiter.success(key)
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "user.signup", ActorType: "anonymous", TargetType: "user", TargetID: result.User.ID})
	s.sendVerification(r.Context(), result.User, result.VerificationToken)
	writeJSON(w, http.StatusAccepted, map[string]any{"message": genericSignupMessage})
}

func (s *Server) userLogin(w http.ResponseWriter, r *http.Request) {
	key := peerIP(r)
	if s.userLoginLimiter.locked(key) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many login attempts. Try again later.")
		return
	}
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	u, err := s.identity.AuthenticatePassword(r.Context(), input.Email, input.Password)
	if err != nil {
		s.userLoginLimiter.recordFailure(key)
		if errors.Is(err, identity.ErrNotVerified) {
			adminError(w, http.StatusForbidden, "email_not_verified", "Verify your email before signing in.")
			return
		}
		adminError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid email or password.")
		return
	}
	s.userLoginLimiter.success(key)
	session, err := s.identity.CreateUserSession(r.Context(), u)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "internal_error", "Could not create session.")
		return
	}
	s.setUserSessionCookie(w, r, session.Token, session.ExpiresAt)
	s.recordAccountAudit(r.Context(), u.AccountID, store.AuditEvent{Event: "user.login", ActorType: "user", ActorID: u.ID})
	writeJSON(w, http.StatusOK, userSessionPayload(session))
}

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(userSessionCookie)
		if err != nil || s.identity == nil {
			adminError(w, http.StatusUnauthorized, "unauthorized", "User authentication required.")
			return
		}
		session, ok := s.identity.GetUserSession(r.Context(), cookie.Value)
		if !ok {
			adminError(w, http.StatusUnauthorized, "unauthorized", "User authentication required.")
			return
		}
		s.setUserSessionCookie(w, r, cookie.Value, session.ExpiresAt)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.identity.CheckUserCSRF(session, r.Header.Get("X-CSRF-Token")) {
			adminError(w, http.StatusForbidden, "csrf_failed", "A valid CSRF token is required.")
			return
		}
		ctx := context.WithValue(r.Context(), userSessionKey, session)
		ctx = context.WithValue(ctx, userKey, session.User)
		ctx = context.WithValue(ctx, accountKey, session.User.AccountID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) userSessionStatus(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(userSessionKey).(identity.UserSession)
	writeJSON(w, http.StatusOK, userSessionPayload(session))
}

func (s *Server) userLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(userSessionCookie); err == nil {
		s.identity.DeleteUserSession(cookie.Value)
	}
	s.clearCookie(w, userSessionCookie)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) verifyEmail(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	u, err := s.identity.ConsumeVerification(r.Context(), input.Token)
	if err != nil {
		adminError(w, http.StatusBadRequest, "invalid_verification", "This verification link is invalid or expired.")
		return
	}
	s.recordAccountAudit(r.Context(), u.AccountID, store.AuditEvent{Event: "user.email_verified", ActorType: "user", ActorID: u.ID})
	writeJSON(w, http.StatusOK, map[string]any{"verified": true})
}

func (s *Server) resendVerification(w http.ResponseWriter, r *http.Request) {
	key := peerIP(r)
	if s.recoveryLimiter.locked(key) {
		writeJSON(w, http.StatusAccepted, map[string]any{"message": genericSignupMessage})
		return
	}
	var input struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	u, token, err := s.identity.IssueVerification(r.Context(), input.Email)
	if err == nil && token != "" {
		s.sendVerification(r.Context(), u, token)
	}
	s.recoveryLimiter.success(key)
	writeJSON(w, http.StatusAccepted, map[string]any{"message": genericSignupMessage})
}

func (s *Server) requestPasswordReset(w http.ResponseWriter, r *http.Request) {
	key := peerIP(r)
	if s.recoveryLimiter.locked(key) {
		writeJSON(w, http.StatusAccepted, map[string]any{"message": genericResetMessage})
		return
	}
	var input struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	u, token, err := s.identity.IssuePasswordReset(r.Context(), input.Email)
	if err == nil && token != "" {
		s.sendPasswordReset(r.Context(), u, token)
	}
	s.recoveryLimiter.success(key)
	writeJSON(w, http.StatusAccepted, map[string]any{"message": genericResetMessage})
}

func (s *Server) confirmPasswordReset(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	u, err := s.identity.ConsumePasswordReset(r.Context(), input.Token, input.Password)
	if err != nil {
		if errors.Is(err, identity.ErrWeakPassword) || errors.Is(err, identity.ErrPasswordTooLong) {
			adminError(w, http.StatusBadRequest, "invalid_password", "Password must be between 12 and 1024 bytes.")
			return
		}
		adminError(w, http.StatusBadRequest, "invalid_reset", "This password-reset link is invalid or expired.")
		return
	}
	s.recordAccountAudit(r.Context(), u.AccountID, store.AuditEvent{Event: "user.password_reset", ActorType: "user", ActorID: u.ID})
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

func (s *Server) setUserSessionCookie(w http.ResponseWriter, _ *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: userSessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: maxAge(expires)})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func maxAge(expires time.Time) int {
	seconds := int(time.Until(expires).Seconds())
	if seconds < 1 {
		return 1
	}
	return seconds
}

func userSessionPayload(session identity.UserSession) map[string]any {
	return map[string]any{"authenticated": true, "email": session.User.Email, "username": session.User.Email, "csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt.UTC(), "account_id": session.User.AccountID}
}

func validEmail(value string) bool {
	value = identity.NormalizeEmail(value)
	return value != "" && strings.Contains(value, "@") && !strings.ContainsAny(value, "\r\n")
}

func (s *Server) sendVerification(ctx context.Context, user identity.User, token string) {
	if s.mailer == nil || s.config.PublicURL == "" {
		return
	}
	message := mailer.Message{To: user.Email, Subject: "Verify your Tiller account", Text: fmt.Sprintf("Verify your Tiller account:\n\n%s/verify-email?token=%s\n\nThis link expires in 24 hours.", s.config.PublicURL, token)}
	if err := s.mailer.Send(ctx, message); err != nil && s.logger != nil {
		s.logger.Warn("verification mail unavailable", "error_class", fmt.Sprintf("%T", err))
	}
}

func (s *Server) sendPasswordReset(ctx context.Context, user identity.User, token string) {
	if s.mailer == nil || s.config.PublicURL == "" {
		return
	}
	message := mailer.Message{To: user.Email, Subject: "Reset your Tiller password", Text: fmt.Sprintf("Reset your Tiller password:\n\n%s/reset-password?token=%s\n\nThis link expires in 1 hour.", s.config.PublicURL, token)}
	if err := s.mailer.Send(ctx, message); err != nil && s.logger != nil {
		s.logger.Warn("password reset mail unavailable", "error_class", fmt.Sprintf("%T", err))
	}
}

// recordAccountAudit writes an account-scoped audit event best-effort. Audit is
// security history, so a write failure is logged loudly, but it must never fail
// the user operation that triggered it (login, verification, reset, deletion).
func (s *Server) recordAccountAudit(ctx context.Context, accountID string, event store.AuditEvent) {
	if err := s.storeHandle().For(accountID).RecordAccountAudit(ctx, event); err != nil && s.logger != nil {
		s.logger.Error("account audit write failed", "event", event.Event, "error_class", fmt.Sprintf("%T", err))
	}
}

// recordPlatformAudit writes a platform audit event best-effort with the same
// contract as recordAccountAudit.
func (s *Server) recordPlatformAudit(ctx context.Context, event store.AuditEvent) {
	if err := s.storeHandle().RecordPlatformAudit(ctx, event); err != nil && s.logger != nil {
		s.logger.Error("platform audit write failed", "event", event.Event, "error_class", fmt.Sprintf("%T", err))
	}
}
