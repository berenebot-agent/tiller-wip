package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/store"
)

const (
	genericEmailChangeMessage = "If the new address can receive mail, a confirmation message will arrive shortly."
	genericAccountDeleted     = "Your account and its data have been deleted."
)

// rawUserSessionToken returns the raw session token from the request cookie.
// UserSession.Token is only populated when a session is created, so a cached
// session's Token field is empty; sensitive account operations need the raw
// value to identify and retain the initiating session.
func rawUserSessionToken(r *http.Request) string {
	if cookie, err := r.Cookie(userSessionCookie); err == nil {
		return cookie.Value
	}
	return ""
}

// accountProfile returns the identity and account summary for the Account page.
func (s *Server) accountProfile(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userKey).(identity.User)
	profile, err := s.identity.AccountProfile(r.Context(), user.ID)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load account details.")
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

// changeOwnPassword re-authenticates and changes the password, then revokes
// every other session.
func (s *Server) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(userSessionKey).(identity.UserSession)
	var input struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if _, err := s.identity.ChangePassword(r.Context(), session.User.ID, rawUserSessionToken(r), input.CurrentPassword, input.NewPassword); err != nil {
		switch {
		case errors.Is(err, identity.ErrWeakPassword), errors.Is(err, identity.ErrPasswordTooLong):
			adminError(w, http.StatusBadRequest, "invalid_password", "Password must be between 12 and 1024 bytes.")
		case errors.Is(err, identity.ErrInvalidSession):
			adminError(w, http.StatusUnauthorized, "unauthorized", "User authentication required.")
		default:
			adminError(w, http.StatusUnauthorized, "invalid_credentials", "Your current password is incorrect.")
		}
		return
	}
	s.recordAccountAudit(r.Context(), session.User.AccountID, store.AuditEvent{Event: "user.password_changed", ActorType: "user", ActorID: session.User.ID})
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.user_password_changed", ActorType: "user", TargetType: "account", TargetID: session.User.AccountID})
	writeJSON(w, http.StatusOK, map[string]any{"changed": true})
}

// requestOwnEmailChange starts the verify-new-first email change flow. The
// external response is generic; an address already owned by another account is
// silently dropped (no mail) to avoid mailing another customer.
func (s *Server) requestOwnEmailChange(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(userSessionKey).(identity.UserSession)
	var input struct {
		NewEmail string `json:"new_email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.identity.VerifyPassword(r.Context(), session.User.ID, input.Password); err != nil {
		adminError(w, http.StatusUnauthorized, "invalid_credentials", "Your password is incorrect.")
		return
	}
	if !validEmail(input.NewEmail) {
		adminError(w, http.StatusBadRequest, "invalid_request", "Enter a valid email address.")
		return
	}
	if _, err := s.identity.RequestEmailChange(r.Context(), session.User.ID, rawUserSessionToken(r), input.NewEmail); err != nil {
		switch {
		case errors.Is(err, identity.ErrEmailUnchanged):
			adminError(w, http.StatusBadRequest, "email_unchanged", "That is already your email address.")
			return
		case errors.Is(err, identity.ErrEmailTaken):
			// Enumeration-resistant: accept generically, send nothing.
			writeJSON(w, http.StatusAccepted, map[string]any{"message": genericEmailChangeMessage})
			return
		case errors.Is(err, identity.ErrNotFound):
			adminError(w, http.StatusBadRequest, "invalid_request", "Enter a valid email address.")
			return
		default:
			adminError(w, http.StatusInternalServerError, "internal_error", "Could not start the email change.")
			return
		}
	}
	s.recordAccountAudit(r.Context(), session.User.AccountID, store.AuditEvent{Event: "user.email_change_requested", ActorType: "user", ActorID: session.User.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{"message": genericEmailChangeMessage})
}

// confirmEmailChange is public: the token arrives from the new address and
// carries the initiating session selector so confirmation keeps that session.
func (s *Server) confirmEmailChange(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	u, err := s.identity.ConfirmEmailChange(r.Context(), input.Token)
	if err != nil {
		if errors.Is(err, identity.ErrEmailTaken) {
			adminError(w, http.StatusConflict, "email_taken", "That email address is no longer available.")
			return
		}
		adminError(w, http.StatusBadRequest, "invalid_change", "This email-change link is invalid or expired.")
		return
	}
	s.recordAccountAudit(r.Context(), u.AccountID, store.AuditEvent{Event: "user.email_changed", ActorType: "user", ActorID: u.ID})
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.user_email_changed", ActorType: "user", TargetType: "account", TargetID: u.AccountID})
	writeJSON(w, http.StatusOK, map[string]any{"changed": true, "email": u.Email})
}

// revokeOwnSessions logs the user out everywhere, including this device.
func (s *Server) revokeOwnSessions(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(userSessionKey).(identity.UserSession)
	if err := s.identity.RevokeAllUserSessions(r.Context(), session.User.ID); err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not revoke sessions.")
		return
	}
	s.recordAccountAudit(r.Context(), session.User.AccountID, store.AuditEvent{Event: "user.sessions_revoked", ActorType: "user", ActorID: session.User.ID})
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.user_sessions_revoked", ActorType: "user", TargetType: "account", TargetID: session.User.AccountID})
	s.clearCookie(w, userSessionCookie)
	w.WriteHeader(http.StatusNoContent)
}

// deleteOwnAccount is instant self-service deletion. It re-authenticates, then
// runs the same synchronous purge as operator deletion. The account audit event
// is written before the purge (it survives deletion); the platform event after.
func (s *Server) deleteOwnAccount(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(userSessionKey).(identity.UserSession)
	var input struct {
		Password string `json:"password"`
		Confirm  string `json:"confirm"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !strings.EqualFold(identity.NormalizeEmail(input.Confirm), identity.NormalizeEmail(session.User.Email)) {
		adminError(w, http.StatusBadRequest, "confirmation_required", "Type your email address to confirm deletion.")
		return
	}
	if err := s.identity.VerifyPassword(r.Context(), session.User.ID, input.Password); err != nil {
		adminError(w, http.StatusUnauthorized, "invalid_credentials", "Your password is incorrect.")
		return
	}
	accountID := session.User.AccountID
	s.recordAccountAudit(r.Context(), accountID, store.AuditEvent{Event: "user.account_delete_requested", ActorType: "user", ActorID: session.User.ID})
	if err := s.purgeAccount(r.Context(), accountID); err != nil {
		adminError(w, http.StatusInternalServerError, "delete_failed", "Could not delete your account.")
		return
	}
	s.recordPlatformAudit(r.Context(), store.AuditEvent{Event: "platform.account_deleted", ActorType: "user", TargetType: "account", TargetID: accountID})
	s.clearCookie(w, userSessionCookie)
	writeJSON(w, http.StatusOK, map[string]any{"message": genericAccountDeleted})
}

// purgeAccount runs the synchronous, terminal account purge. It establishes the
// terminal `deleting` status itself, so both operator deletion and self-service
// deletion share the same invariant: a client API key can never re-authenticate
// against an active account while the purge is in flight. All coordination
// state is durable, so it is safe to call again after a partial failure.
func (s *Server) purgeAccount(ctx context.Context, accountID string) error {
	if err := s.identity.SetAccountStatus(ctx, accountID, "deleting"); err != nil {
		return err
	}
	s.identity.InvalidateAccount(accountID)
	s.clients.InvalidateAccount(accountID)
	if err := s.storeHandle().BeginActivityCleanup(accountID, ""); err != nil {
		return err
	}
	if err := s.storeHandle().For(accountID).DeleteAccountResources(ctx); err != nil {
		return err
	}
	if err := s.finalizeActivityCleanup(ctx, accountID, ""); err != nil {
		return err
	}
	return s.identity.DeleteAccountIdentity(ctx, accountID)
}
