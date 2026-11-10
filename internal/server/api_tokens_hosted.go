package server

import (
	"context"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/mailoutbox"
)

// Hosted management-token policy. Local installs keep the unrestricted
// behaviour; a hosted account shares infrastructure with other tenants, so
// tokens are tighter: capped in number, tier and lifetime, and rate-limited per account. See
// docs/hosted_decisions.md ("Hosted management API tokens").
const (
	// hostedMaxManagementTokens caps browser-managed tokens per account.
	hostedMaxManagementTokens = 10
	// hostedMaxManagementScopeTier is the highest tier a hosted token may hold.
	// Destructive (delete) operations stay with the interactive session.
	hostedMaxManagementScopeTier = auth.ScopeWrite
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
