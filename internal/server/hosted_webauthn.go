package server

import (
	"errors"
	"net/http"

	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/store"
)

// challengeHeader carries the one-use ceremony token between the begin and
// finish steps. A header keeps the finish body reserved for the raw
// PublicKeyCredential JSON the WebAuthn library parses.
const challengeHeader = "X-WebAuthn-Challenge"

// passkeyUnavailable reports a clear 501 when the deployment has no public
// origin (local mode) and therefore no passkey support.
func (s *Server) passkeyUnavailable(w http.ResponseWriter) bool {
	if s.identity == nil {
		adminError(w, http.StatusNotImplemented, "passkeys_unavailable", "Passkeys are not available for this deployment.")
		return true
	}
	return false
}

// passkeyLoginBegin starts a usernameless, discoverable-credential login.
func (s *Server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireSameOrigin(w, r) {
		return
	}
	key := clientIP(r, s.config.TrustedProxy)
	if s.userLoginIPLimiter.locked(key) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many login attempts. Try again later.")
		return
	}
	opts, err := s.identity.BeginPasskeyLogin()
	if err != nil {
		if errors.Is(err, identity.ErrWebAuthnUnavailable) {
			adminError(w, http.StatusNotImplemented, "passkeys_unavailable", "Passkeys are not available for this deployment.")
			return
		}
		adminError(w, http.StatusInternalServerError, "internal_error", "Could not start passkey sign-in.")
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

// passkeyLoginFinish validates the assertion, records the credential use, and
// mints a normal session.
func (s *Server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireSameOrigin(w, r) {
		return
	}
	key := clientIP(r, s.config.TrustedProxy)
	if s.userLoginIPLimiter.locked(key) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many login attempts. Try again later.")
		return
	}
	u, err := s.identity.FinishPasskeyLogin(r.Context(), r.Header.Get(challengeHeader), r)
	if err != nil {
		s.userLoginIPLimiter.recordFailure(key)
		switch {
		case errors.Is(err, identity.ErrWebAuthnUnavailable):
			adminError(w, http.StatusNotImplemented, "passkeys_unavailable", "Passkeys are not available for this deployment.")
		case errors.Is(err, identity.ErrNotVerified):
			adminError(w, http.StatusForbidden, "email_not_verified", "Verify your email before signing in.")
		default:
			adminError(w, http.StatusUnauthorized, "invalid_credentials", "Passkey sign-in failed.")
		}
		return
	}
	session, err := s.identity.CreateUserSession(r.Context(), u)
	if err != nil {
		if errors.Is(err, identity.ErrStaleAuthentication) {
			adminError(w, http.StatusUnauthorized, "invalid_credentials", "Passkey sign-in failed.")
			return
		}
		adminError(w, http.StatusInternalServerError, "internal_error", "Could not create session.")
		return
	}
	s.setUserSessionCookie(w, r, session.Token, session.ExpiresAt)
	s.recordAccountAudit(r.Context(), u.AccountID, store.AuditEvent{Event: "user.login", ActorType: "user", ActorID: u.ID, Metadata: map[string]string{"method": "passkey"}})
	writeJSON(w, http.StatusOK, userSessionPayload(session))
}

// passkeyRegisterBegin starts a registration ceremony for the signed-in user.
func (s *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if s.passkeyUnavailable(w) {
		return
	}
	user := r.Context().Value(userKey).(identity.User)
	opts, err := s.identity.BeginPasskeyRegistration(r.Context(), user)
	if err != nil {
		if errors.Is(err, identity.ErrWebAuthnUnavailable) {
			adminError(w, http.StatusNotImplemented, "passkeys_unavailable", "Passkeys are not available for this deployment.")
			return
		}
		adminError(w, http.StatusInternalServerError, "internal_error", "Could not start passkey registration.")
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

// passkeyRegisterFinish validates the response, stores the passkey, and — when
// the user asked to make it their only sign-in method — disables password
// sign-in. The passkey is stored first, so the account always keeps a method.
func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if s.passkeyUnavailable(w) {
		return
	}
	user := r.Context().Value(userKey).(identity.User)
	name := r.URL.Query().Get("name")
	if err := s.identity.FinishPasskeyRegistration(r.Context(), user, r.Header.Get(challengeHeader), name, r); err != nil {
		switch {
		case errors.Is(err, identity.ErrAlreadyUsed):
			adminError(w, http.StatusConflict, "passkey_exists", "This passkey is already registered.")
		case errors.Is(err, identity.ErrInvalidToken):
			adminError(w, http.StatusBadRequest, "invalid_challenge", "The registration expired. Try again.")
		default:
			adminError(w, http.StatusBadRequest, "passkey_failed", "Passkey registration failed.")
		}
		return
	}
	// Optionally make this the only sign-in method.
	passwordOnly := false
	if r.URL.Query().Get("only") == "1" {
		if err := s.identity.SetPasswordSignInEnabled(r.Context(), user.ID, false); err != nil {
			adminError(w, http.StatusInternalServerError, "internal_error", "Passkey saved, but password sign-in could not be disabled.")
			return
		}
		passwordOnly = true
	}
	s.recordAccountAudit(r.Context(), user.AccountID, store.AuditEvent{Event: "user.passkey_added", ActorType: "user", ActorID: user.ID})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "password_only": passwordOnly})
}

// passkeyList returns the signed-in user's passkeys.
func (s *Server) passkeyList(w http.ResponseWriter, r *http.Request) {
	if s.passkeyUnavailable(w) {
		return
	}
	user := r.Context().Value(userKey).(identity.User)
	keys, err := s.identity.ListPasskeys(r.Context(), user.ID)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load passkeys.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": keys})
}

// passkeyRename changes a passkey's label.
func (s *Server) passkeyRename(w http.ResponseWriter, r *http.Request) {
	if s.passkeyUnavailable(w) {
		return
	}
	user := r.Context().Value(userKey).(identity.User)
	var input struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := decodeJSONLimit(w, r, &input, authRequestMaxBytes); err != nil {
		respondDecodeError(w, err)
		return
	}
	if err := s.identity.RenamePasskey(r.Context(), user.ID, input.ID, input.Name); err != nil {
		if errors.Is(err, identity.ErrPasskeyNotFound) {
			adminError(w, http.StatusNotFound, "passkey_not_found", "That passkey no longer exists.")
			return
		}
		adminError(w, http.StatusBadRequest, "invalid_request", "Could not rename the passkey.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"renamed": true})
}

// passkeyDelete removes a passkey, refusing when it is the only sign-in method.
func (s *Server) passkeyDelete(w http.ResponseWriter, r *http.Request) {
	if s.passkeyUnavailable(w) {
		return
	}
	user := r.Context().Value(userKey).(identity.User)
	var input struct {
		ID string `json:"id"`
	}
	if err := decodeJSONLimit(w, r, &input, authRequestMaxBytes); err != nil {
		respondDecodeError(w, err)
		return
	}
	err := s.identity.DeletePasskey(r.Context(), user.ID, input.ID)
	switch {
	case errors.Is(err, identity.ErrLastAuthMethod):
		adminError(w, http.StatusConflict, "last_auth_method", "This is your only sign-in method. Add a password or another passkey first.")
	case errors.Is(err, identity.ErrPasskeyNotFound):
		adminError(w, http.StatusNotFound, "passkey_not_found", "That passkey no longer exists.")
	case err != nil:
		adminError(w, http.StatusInternalServerError, "internal_error", "Could not remove the passkey.")
	default:
		s.recordAccountAudit(r.Context(), user.AccountID, store.AuditEvent{Event: "user.passkey_removed", ActorType: "user", ActorID: user.ID})
		writeJSON(w, http.StatusOK, map[string]any{"removed": true})
	}
}

// setPasswordSignIn enables or disables password sign-in for the signed-in user
// outside the add-passkey flow (e.g. turning it back on).
func (s *Server) setPasswordSignIn(w http.ResponseWriter, r *http.Request) {
	if s.passkeyUnavailable(w) {
		return
	}
	user := r.Context().Value(userKey).(identity.User)
	var input struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSONLimit(w, r, &input, authRequestMaxBytes); err != nil {
		respondDecodeError(w, err)
		return
	}
	if err := s.identity.SetPasswordSignInEnabled(r.Context(), user.ID, input.Enabled); err != nil {
		if errors.Is(err, identity.ErrLastAuthMethod) {
			adminError(w, http.StatusConflict, "last_auth_method", "Add a passkey before disabling password sign-in.")
			return
		}
		adminError(w, http.StatusInternalServerError, "internal_error", "Could not update password sign-in.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": input.Enabled})
}
