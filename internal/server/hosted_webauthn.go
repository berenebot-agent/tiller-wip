package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/store"
)

// challengeHeader carries the one-use ceremony token between the begin and
// finish steps. A header keeps the finish body reserved for the raw
// PublicKeyCredential JSON the WebAuthn library parses.
const challengeHeader = "X-WebAuthn-Challenge"

// webauthnFinishMaxBytes bounds the finish bodies. A registration response
// (attestation object + client data) is a few KiB at most; 64 KiB leaves ample
// headroom while still refusing a body that could hold a connection.
const webauthnFinishMaxBytes = 64 << 10

// passkeyUnavailable reports a clear 501 when the deployment has no public
// origin (local mode) and therefore no passkey support. It checks live
// configuration, not just the store pointer, so an unconfigured deployment
// cannot list, rename, delete, or toggle passkeys either.
func (s *Server) passkeyUnavailable(w http.ResponseWriter) bool {
	if s.identity == nil || !s.identity.PasskeysEnabled() {
		adminError(w, http.StatusNotImplemented, "passkeys_unavailable", "Passkeys are not available for this deployment.")
		return true
	}
	return false
}

// passkeyFinishBody bounds the body and applies the same read deadline every
// other JSON endpoint uses, buffering the body so the WebAuthn library can
// still parse it from a fresh reader. Without this the library reads
// request.Body directly with no cap, deadline, or admission gate.
func (s *Server) passkeyFinishBody(w http.ResponseWriter, r *http.Request) bool {
	// The body is a JSON PublicKeyCredential. Requiring the JSON content type
	// closes the same cross-site-form "JSON CSRF" surface decodeJSONLimit
	// guards for every other auth endpoint.
	if !jsonContentType(r.Header.Get("Content-Type")) {
		adminError(w, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
		return false
	}
	if !s.bodyReads.acquire() {
		adminError(w, http.StatusServiceUnavailable, "body_read_busy", "The router is busy reading request bodies. Retry shortly.")
		return false
	}
	defer s.bodyReads.release()
	r.Body = http.MaxBytesReader(w, r.Body, webauthnFinishMaxBytes)
	var body []byte
	if err := withBodyReadDeadline(w, func() error {
		var readErr error
		body, readErr = io.ReadAll(r.Body)
		return readErr
	}); err != nil {
		if isTimeoutError(err) {
			adminError(w, http.StatusRequestTimeout, "request_timeout", "The request body was not received in time.")
		} else {
			adminError(w, http.StatusBadRequest, "invalid_request", "The passkey response is invalid or too large.")
		}
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return true
}

// passkeyChallengeToken reads the one-use ceremony token from the request
// header.
func passkeyChallengeToken(r *http.Request) string {
	return r.Header.Get(challengeHeader)
}

// passkeyLoginBegin starts a usernameless, discoverable-credential login.
func (s *Server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireSameOrigin(w, r) {
		return
	}
	key := clientIP(r, s.config.TrustedProxy)
	// begin is unauthenticated, so each call is charged against a per-IP fixed
	// window. Without this an attacker can churn ceremonies and the
	// process-wide challenge store; a dedicated budget (not the shared
	// login-failure lockout) keeps a legitimate user's retries from tripping
	// the account lockout.
	if !s.passkeyBeginLimiter.allowAttempt(key) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many passkey attempts. Try again later.")
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
	if !s.passkeyFinishBody(w, r) {
		return
	}
	assertion, err := s.identity.FinishPasskeyLogin(r.Context(), passkeyChallengeToken(r), r)
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
	if assertion.CloneWarning {
		s.logger.Warn("passkey signature counter did not advance", "user_id", assertion.User.ID)
	}
	if assertion.RecordErr != nil {
		s.logger.Warn("passkey use could not be recorded", "user_id", assertion.User.ID, "error_class", fmt.Sprintf("%T", assertion.RecordErr))
	}
	u := assertion.User
	if s.config.Mode != config.ModeHosted {
		// Local mode has no user_sessions boundary; the passkey login bridges to
		// the existing admin session so requireAdmin and LocalAccountID scoping
		// are unchanged. The session store is nil only in hosted mode, which is
		// not this branch.
		if s.sessions == nil {
			adminError(w, http.StatusInternalServerError, "internal_error", "Could not create session.")
			return
		}
		session, err := s.sessions.Create()
		if err != nil {
			adminError(w, http.StatusInternalServerError, "internal_error", "Could not create session.")
			return
		}
		s.setSessionCookie(w, r, session.Token, session.ExpiresAt)
		s.notifyAdminEvent(u.AccountID, eventAdminLogin, fmt.Sprintf("User: %s\nMethod: passkey", u.Email))
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": s.adminUsername(), "csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt.UTC()})
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
	if !s.passkeyFinishBody(w, r) {
		return
	}
	if err := s.identity.FinishPasskeyRegistration(r.Context(), user, passkeyChallengeToken(r), name, r); err != nil {
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
	// Optionally make this the only sign-in method. The passkey is already
	// saved at this point, so a refusal must say so rather than implying the
	// whole operation failed. Local mode always keeps password sign-in
	// available (the environment credential is the operator's recovery path),
	// so the request is honoured only in hosted mode.
	passwordOnly := false
	if r.URL.Query().Get("only") == "1" && s.config.Mode == config.ModeHosted {
		if err := s.identity.SetPasswordSignInEnabled(r.Context(), user.ID, rawUserSessionToken(r), false); err != nil {
			if errors.Is(err, identity.ErrLastAuthMethod) {
				adminError(w, http.StatusConflict, "last_auth_method", "Passkey saved, but it is your only sign-in method.")
				return
			}
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
	s.recordAccountAudit(r.Context(), user.AccountID, store.AuditEvent{Event: "user.passkey_renamed", ActorType: "user", ActorID: user.ID})
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
// outside the add-passkey flow (e.g. turning it back on). The initiating
// session is retained so the UI does not get signed out by the change.
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
	// Local mode keeps password sign-in permanently available as the operator's
	// recovery path; it can never be disabled there.
	if !input.Enabled && s.config.Mode != config.ModeHosted {
		adminError(w, http.StatusConflict, "password_required", "Password sign-in cannot be disabled on a local install; it is the recovery path.")
		return
	}
	if err := s.identity.SetPasswordSignInEnabled(r.Context(), user.ID, rawUserSessionToken(r), input.Enabled); err != nil {
		switch {
		case errors.Is(err, identity.ErrLastAuthMethod):
			adminError(w, http.StatusConflict, "last_auth_method", "Add a passkey before disabling password sign-in.")
		case errors.Is(err, identity.ErrPasswordDisabled):
			adminError(w, http.StatusConflict, "password_disabled", "Password sign-in cannot be enabled while Google is linked.")
		default:
			adminError(w, http.StatusInternalServerError, "internal_error", "Could not update password sign-in.")
		}
		return
	}
	event := "user.password_signin_disabled"
	if input.Enabled {
		event = "user.password_signin_enabled"
	}
	s.recordAccountAudit(r.Context(), user.AccountID, store.AuditEvent{Event: event, ActorType: "user", ActorID: user.ID})
	writeJSON(w, http.StatusOK, map[string]any{"enabled": input.Enabled})
}

// passkeyReauthBegin starts a passkey assertion for the signed-in user, used to
// re-authenticate before sensitive account operations (change password, change
// email, delete account) for accounts without a usable password.
func (s *Server) passkeyReauthBegin(w http.ResponseWriter, r *http.Request) {
	if s.passkeyUnavailable(w) {
		return
	}
	user := r.Context().Value(userKey).(identity.User)
	opts, err := s.identity.BeginPasskeyReauth(r.Context(), user)
	if err != nil {
		if errors.Is(err, identity.ErrWebAuthnUnavailable) {
			adminError(w, http.StatusNotImplemented, "passkeys_unavailable", "Passkeys are not available for this deployment.")
			return
		}
		if errors.Is(err, identity.ErrPasskeyNotFound) {
			adminError(w, http.StatusConflict, "passkey_not_found", "No passkey is registered for this account.")
			return
		}
		adminError(w, http.StatusInternalServerError, "internal_error", "Could not start passkey confirmation.")
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

// passkeyReauthFinish validates the assertion and records a short-lived,
// one-use re-auth grant for this session, mirroring the Google re-auth grant.
// The grant is consumed by sensitive account operations in place of a password.
func (s *Server) passkeyReauthFinish(w http.ResponseWriter, r *http.Request) {
	if s.passkeyUnavailable(w) {
		return
	}
	user := r.Context().Value(userKey).(identity.User)
	if !s.passkeyFinishBody(w, r) {
		return
	}
	if err := s.identity.FinishPasskeyReauth(r.Context(), user, passkeyChallengeToken(r), r); err != nil {
		if errors.Is(err, identity.ErrInvalidToken) || strings.Contains(err.Error(), "Session has Expired") {
			adminError(w, http.StatusBadRequest, "invalid_challenge", "The confirmation expired. Try again.")
			return
		}
		adminError(w, http.StatusUnauthorized, "invalid_credentials", "Passkey confirmation failed.")
		return
	}
	s.grantPasskeyReauth(rawUserSessionToken(r))
	s.recordAccountAudit(r.Context(), user.AccountID, store.AuditEvent{Event: "user.passkey_reauthenticated", ActorType: "user", ActorID: user.ID})
	writeJSON(w, http.StatusOK, map[string]any{"confirmed": true})
}
