package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

// setupMinPasswordBytes is the first-run wizard's floor. It is deliberately
// lower than the hosted-account passphrase floor (12 bytes): local mode is an
// appliance, the operator is standing at the machine, and the value is theirs
// to choose. Environment-var credentials remain unvalidated for backwards
// compatibility.
const setupMinPasswordBytes = 8

// setupUsernameMaxLen bounds the stored admin username.
const setupUsernameMaxLen = 64

// setup writes the first admin credential. It is reachable only while the
// instance has no credential at all (no environment admin and no stored hash),
// so it is the one unauthenticated mutating endpoint in local mode. Protection
// is layered: same-origin (requireSameOrigin), a per-IP attempt budget
// (setupLimiter), strict input validation, and a one-shot transactional write
// (SetCredential) that can never overwrite an existing credential. After the
// claim the route 404s.
type setupInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if s.config.Mode == config.ModeHosted || !s.setupRequired() {
		// Not a first-run install (already configured, environment admin, or
		// hosted). Behave as if the route does not exist.
		adminError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	key := s.requestClientIP(r)
	if !s.setupLimiter.allowAttempt(key) {
		w.Header().Set("Retry-After", "60")
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many setup attempts. Try again later.")
		return
	}
	var input setupInput
	if err := decodeJSONLimit(w, r, &input, authRequestMaxBytes); err != nil {
		respondDecodeError(w, err)
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if err := validateSetupUsername(input.Username); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_username", err.Error())
		return
	}
	if err := validateSetupPassword(input.Password); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_password", err.Error())
		return
	}
	if err := s.sessions.SetCredential(input.Username, input.Password); err != nil {
		switch {
		case errors.Is(err, auth.ErrCredentialAlreadySet):
			adminError(w, http.StatusConflict, "already_configured", "This instance is already set up. Sign in instead.")
		case errors.Is(err, auth.ErrPartialCredential):
			adminError(w, http.StatusBadRequest, "invalid_request", "A username and password are required.")
		default:
			adminError(w, http.StatusInternalServerError, "database_error", "Could not save the administrator credential.")
		}
		return
	}
	// Straight to an authenticated session: setup is a claim, and the claimer
	// goes on to the onboarding wizard without a second login.
	session, err := s.sessions.Create()
	if err != nil {
		adminError(w, http.StatusInternalServerError, "internal_error", "Credential saved, but a session could not be created. Sign in with the new credentials.")
		return
	}
	s.setSessionCookie(w, r, session.Token, session.ExpiresAt)
	s.notifyAdminEvent(database.LocalAccountID, eventAdminLogin, fmt.Sprintf("User: %s\nIP: %s", input.Username, clientIP(r, s.config.TrustedProxy)))
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"username":      input.Username,
		"csrf_token":    session.CSRFToken,
		"expires_at":    session.ExpiresAt.UTC(),
	})
}

// setupRequired reports whether the instance still needs first-run credential
// setup: local mode, no environment admin, and no stored credential. It is the
// gate for both the setup endpoint and runtime's setup_required signal.
func (s *Server) setupRequired() bool {
	if s.config.Mode == config.ModeHosted || s.sessions == nil {
		return false
	}
	if s.config.TillerUser != "" || s.config.TillerUserPassword != "" {
		return false
	}
	return !s.sessions.CredentialConfigured()
}

// validateSetupUsername enforces a simple, storage-safe username: non-empty,
// bounded, printable, and without surrounding whitespace (already trimmed).
func validateSetupUsername(username string) error {
	if username == "" {
		return errors.New("Enter a username.")
	}
	if len(username) > setupUsernameMaxLen {
		return fmt.Errorf("Username must be %d characters or fewer.", setupUsernameMaxLen)
	}
	for _, r := range username {
		if r < 0x20 || r == 0x7f {
			return errors.New("Username may not contain control characters.")
		}
	}
	return nil
}

// validateSetupPassword enforces the first-run password floor. The upper bound
// keeps the argon2id input bounded and matches the hosted-account cap.
func validateSetupPassword(password string) error {
	if utf8.RuneCountInString(password) == 0 {
		return errors.New("Enter a password.")
	}
	if len(password) < setupMinPasswordBytes {
		return fmt.Errorf("Password must be at least %d characters.", setupMinPasswordBytes)
	}
	if len(password) > 1024 {
		return errors.New("Password must be 1024 characters or fewer.")
	}
	return nil
}
