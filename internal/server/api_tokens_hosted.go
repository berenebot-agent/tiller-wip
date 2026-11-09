package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/mailoutbox"
)

// Hosted management-token policy. Local installs keep the unrestricted
// behaviour; a hosted account shares infrastructure with other tenants, so
// tokens are tighter: capped in number, tier and lifetime, minted and widened
// only after re-authentication, and rate-limited per account. See
// docs/hosted_decisions.md ("Hosted management API tokens").
const (
	// hostedMaxManagementTokens caps browser-managed tokens per account.
	hostedMaxManagementTokens = 10
	// hostedMaxManagementScopeTier is the highest tier a hosted token may hold.
	// Destructive (delete) operations stay with the interactive session.
	hostedMaxManagementScopeTier = auth.ScopeWrite
	// hostedManagementRequestsPerMinute is the per-account management API
	// request budget, shared by all of the account's tokens.
	hostedManagementRequestsPerMinute = 60
)

func (s *Server) hostedTokens() bool { return s.config.Mode == config.ModeHosted }

// hostedTokenScopeError reports a scope set above the hosted tier ceiling.
func hostedTokenScopeError(scopes map[string]int) string {
	for area, tier := range scopes {
		if tier > hostedMaxManagementScopeTier {
			return "Hosted API tokens are limited to read and write access; area " + area + " requested more."
		}
	}
	return ""
}

// widensManagementScopes reports whether next grants anything beyond current.
func widensManagementScopes(current, next map[string]int) bool {
	for area, tier := range next {
		if tier > current[area] {
			return true
		}
	}
	return false
}

// hostedTokenStepUp requires a fresh proof of identity for sensitive token
// operations (mint, rotate, widen). It accepts the password, or a passkey or
// Google re-authentication already completed in this session, and throttles
// failures per user. It writes the error response and returns false on failure.
func (s *Server) hostedTokenStepUp(w http.ResponseWriter, r *http.Request, password string) bool {
	session, ok := r.Context().Value(userSessionKey).(identity.UserSession)
	if !ok {
		adminError(w, http.StatusUnauthorized, "unauthorized", "User authentication required.")
		return false
	}
	key := session.User.ID
	if s.tokenStepUpLimiter.locked(key) {
		w.Header().Set("Retry-After", "900")
		adminError(w, http.StatusTooManyRequests, "authentication_rate_limited", "Too many failed attempts. Try again later.")
		return false
	}
	profile, err := s.identity.AccountProfile(r.Context(), session.User.ID)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not verify your account details.")
		return false
	}
	var authErr error
	if profile.GoogleLinked && !profile.PasswordEnabled {
		if !s.consumeGoogleReauth(rawUserSessionToken(r)) && !s.consumePasskeyReauth(rawUserSessionToken(r)) {
			authErr = errReauthRequired
		}
	} else if _, err := s.reauthenticateSensitive(r, session.User.ID, password); err != nil {
		authErr = err
	}
	if authErr != nil {
		s.tokenStepUpLimiter.recordFailure(key)
		adminError(w, http.StatusUnauthorized, "reauth_required", "Confirm your identity with your password, passkey, or Google to manage API tokens.")
		return false
	}
	s.tokenStepUpLimiter.success(key)
	return true
}

var errReauthRequired = errors.New("reauthentication required")

// notifyManagementTokenCreated queues a security notice to the account owner.
// It is best-effort: a mail failure never blocks or fails token creation.
func (s *Server) notifyManagementTokenCreated(ctx context.Context, user identity.User, name string) {
	if s.outbox == nil || s.db == nil || user.Email == "" {
		return
	}
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	err = s.outbox.Enqueue(ctx, tx, mailoutbox.QueuedMessage{UserID: user.ID, Type: mailoutbox.TypeAPITokenCreated, Recipient: user.Email, Params: map[string]string{"token_name": name}})
	if err != nil {
		_ = tx.Rollback()
		return
	}
	if tx.Commit() == nil {
		s.outbox.Nudge()
	}
}

// revokeAccountManagementTokens deletes every browser-managed token for an
// account and drops them from the verification cache. "Sign out everywhere"
// calls it when the user opts in; password resets deliberately do not. It
// returns the database error: a failed delete leaves the tokens valid, so the
// caller must not report them revoked. The cache is dropped either way, which
// is harmless because an authenticating token is re-read from the database.
func (s *Server) revokeAccountManagementTokens(ctx context.Context, accountID string) error {
	if !s.hostedTokens() || accountID == "" {
		return nil
	}
	_, err := s.scopeFor(accountID).RevokeAllManagementTokens(ctx)
	s.tokens.InvalidateAccount(accountID)
	return err
}
