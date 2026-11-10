package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/store"
)

// Management API tokens (local and hosted; see api_tokens_hosted.go for the
// hosted policy). A token authenticates the admin REST
// surface with `Authorization: Bearer mt-tr-…` instead of the browser session.
// Token lifecycle management itself is cookie-only: a token can never list,
// create, re-scope, rotate, or revoke tokens — it may only introspect itself.

// errBootManagementTokenNameTaken reports that an ordinary token already holds
// the environment-managed token's name, so the boot seed cannot proceed.
var errBootManagementTokenNameTaken = errors.New("an existing management API token is named \"" + bootManagementTokenName + "\", which is reserved for the TILLER_ADMIN_API_KEY token; unset TILLER_ADMIN_API_KEY and restart, rename or delete that token in the admin UI (API tokens), then set the key again")

func (s *Server) checkBootManagementTokenNameFree() error {
	rows, err := s.scopeFor(database.LocalAccountID).ListManagementTokens(context.Background())
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID != bootManagementTokenID && row.Name == bootManagementTokenName {
			return errBootManagementTokenNameTaken
		}
	}
	return nil
}

// bootManagementTokenID and bootManagementTokenName are the fixed identity of
// the environment-managed credential. A fixed id makes re-seeding idempotent:
// a restart restores the configured credential, and a restart with a new value
// replaces the selector/hash and invalidates the cached old credential.
const (
	bootManagementTokenID   = store.EnvironmentManagementTokenID
	bootManagementTokenName = "boot-admin"
)

// seedManagementToken stores the environment-supplied management token as the
// fixed full-power boot token. The plaintext is hashed (never stored) and its
// hash is published under the authenticator's lock so a re-seed is immediately
// effective. Only the local account is boot-seeded.
func (s *Server) seedManagementToken(plaintext string) error {
	selector, hash, fingerprint, err := auth.SplitManagementToken(plaintext, s.secretHasher)
	if err != nil {
		return err
	}
	scopesJSON, err := auth.ValidateManagementScopes(auth.FullManagementScopes())
	if err != nil {
		return err
	}
	// A token created before the name was reserved can hold it; the upsert
	// would then fail on the (account_id, name) uniqueness rule with an opaque
	// SQLite error. Fail with an actionable message instead. Deliberately no
	// automatic rename (Ben, 2026-11-06: deployments are few, fail loud).
	if err := s.checkBootManagementTokenNameFree(); err != nil {
		return err
	}
	return s.tokens.InvalidateWith(bootManagementTokenID, func() error {
		return s.scopeFor(database.LocalAccountID).UpsertManagementToken(context.Background(), store.CreateManagementTokenInput{
			ID:          bootManagementTokenID,
			Name:        bootManagementTokenName,
			Description: "Environment-managed. Change the configured key and restart to rotate; remove both key configuration sources and restart to revoke.",
			Selector:    selector,
			Hash:        hash,
			Fingerprint: fingerprint,
			Scopes:      scopesJSON,
		})
	})
}

// surface with `Authorization: Bearer mt-tr-…` instead of the browser session.
// Token lifecycle management itself is cookie-only: a token can never list,
// create, re-scope, rotate, or revoke tokens — it may only introspect itself.

type managementTokenView struct {
	EnvironmentManaged bool           `json:"environment_managed"`
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Description        string         `json:"description"`
	Fingerprint        string         `json:"fingerprint"`
	Scopes             map[string]int `json:"scopes"`
	Enabled            bool           `json:"enabled"`
	ExpiresAt          *string        `json:"expires_at"`
	Expired            bool           `json:"expired"`
	LastUsedAt         *string        `json:"last_used_at"`
	CreatedAt          string         `json:"created_at"`
	RotatedAt          *string        `json:"rotated_at"`
	UpdatedAt          string         `json:"updated_at"`
}

func managementTokenViewFromRow(row store.ManagementTokenRow) managementTokenView {
	scopes := auth.ParseManagementScopes(row.Scopes)
	if scopes == nil {
		scopes = map[string]int{}
	}
	v := managementTokenView{
		EnvironmentManaged: row.ID == bootManagementTokenID,
		ID:                 row.ID,
		Name:               row.Name,
		Description:        row.Description,
		Fingerprint:        row.Fingerprint,
		Scopes:             scopes,
		Enabled:            row.Enabled,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
	if row.ExpiresAt.Valid {
		expires := row.ExpiresAt.String
		v.ExpiresAt = &expires
		if exp, err := time.Parse(time.RFC3339Nano, expires); err == nil && !time.Now().Before(exp) {
			v.Expired = true
		}
	}
	if row.LastUsedAt.Valid {
		lastUsed := row.LastUsedAt.String
		v.LastUsedAt = &lastUsed
	}
	if row.RotatedAt.Valid {
		rotated := row.RotatedAt.String
		v.RotatedAt = &rotated
	}
	return v
}

// parseManagementTokenExpiry accepts an expiry as RFC3339 or a date
// (YYYY-MM-DD, stored as midnight UTC). An empty string means "never".
func parseManagementTokenExpiry(value string) (sql.NullString, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return sql.NullString{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t, err = time.Parse("2006-01-02", value)
		if err != nil {
			return sql.NullString{}, errors.New("expiry must be an RFC3339 timestamp or YYYY-MM-DD date")
		}
	}
	if !time.Now().Before(t) {
		return sql.NullString{}, errors.New("expiry must be in the future")
	}
	return sql.NullString{String: t.UTC().Format(time.RFC3339Nano), Valid: true}, nil
}

func validManagementTokenName(name string) bool {
	return name != "" && len(name) <= 100
}

// reservedManagementTokenName reports whether name collides with the
// environment-managed token's name. It is reserved unconditionally (not only
// while TILLER_ADMIN_API_KEY is set), because the key may be added later and
// the boot seed would then fail on the (account_id, name) uniqueness rule.
// Callers pass the already-trimmed name; the match ignores case.
func reservedManagementTokenName(name string) bool {
	return strings.EqualFold(name, bootManagementTokenName)
}

// rejectReservedManagementTokenName writes the error for a reserved name.
func rejectReservedManagementTokenName(w http.ResponseWriter, name string) bool {
	if !reservedManagementTokenName(name) {
		return false
	}
	adminError(w, 400, "reserved_name", fmt.Sprintf("The name %q is reserved for the environment-managed token.", bootManagementTokenName))
	return true
}

func (s *Server) listManagementTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := s.scope(r).ListManagementTokens(r.Context())
	if err != nil {
		adminError(w, 500, "database_error", "Could not list API tokens.")
		return
	}
	views := make([]managementTokenView, 0, len(rows))
	for _, row := range rows {
		views = append(views, managementTokenViewFromRow(row))
	}
	writeJSON(w, 200, map[string]any{"data": views})
}

func (s *Server) createManagementToken(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Scopes      map[string]int `json:"scopes"`
		ExpiresAt   string         `json:"expires_at"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if !validManagementTokenName(input.Name) {
		adminError(w, 400, "invalid_name", "Token names must be 1-100 characters.")
		return
	}
	if rejectReservedManagementTokenName(w, input.Name) {
		return
	}
	if len(input.Description) > 500 {
		adminError(w, 400, "invalid_description", "Descriptions are limited to 500 characters.")
		return
	}
	scopesJSON, err := auth.ValidateManagementScopes(input.Scopes)
	if err != nil {
		adminError(w, 400, "invalid_scopes", err.Error())
		return
	}
	expiresAt, err := parseManagementTokenExpiry(input.ExpiresAt)
	if err != nil {
		adminError(w, 400, "invalid_expiry", err.Error())
		return
	}
	if s.hostedTokens() {
		if msg := hostedTokenScopeError(input.Scopes); msg != "" {
			adminError(w, 400, "invalid_scopes", msg)
			return
		}
		count, err := s.scope(r).CountManagementTokens(r.Context())
		if err != nil {
			adminError(w, 500, "database_error", "Could not create API token.")
			return
		}
		if count >= hostedMaxManagementTokens {
			adminError(w, 409, "token_limit_reached", fmt.Sprintf("Accounts can hold at most %d API tokens. Delete one first.", hostedMaxManagementTokens))
			return
		}
	}
	generated, err := auth.GenerateManagementTokenWithHasher(s.secretHasher)
	if err != nil {
		adminError(w, 500, "internal_error", "Could not create API token.")
		return
	}
	tokenID, err := id.New()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not create API token.")
		return
	}
	err = s.scope(r).CreateManagementToken(r.Context(), store.CreateManagementTokenInput{
		ID:          tokenID,
		Name:        input.Name,
		Description: input.Description,
		Selector:    generated.Selector,
		Hash:        generated.Hash,
		Fingerprint: generated.Fingerprint,
		Scopes:      scopesJSON,
		ExpiresAt:   expiresAt,
	})
	if err != nil {
		if database.IsConstraint(err) {
			adminError(w, 409, "name_conflict", "An API token with that name already exists.")
			return
		}
		adminError(w, 500, "database_error", "Could not create API token.")
		return
	}
	s.recordResourceAudit(r, s.scope(r).AccountID(), store.AuditEvent{
		Event:      "api_token.created",
		TargetType: "api_token",
		TargetID:   tokenID,
		Metadata:   map[string]string{"name": input.Name},
	})
	if s.hostedTokens() {
		if user, ok := r.Context().Value(userKey).(identity.User); ok {
			s.notifyManagementTokenCreated(r.Context(), user, input.Name)
		}
	}
	// The plaintext token is never recorded anywhere; this response is the
	// only place it exists outside the operator's clipboard.
	writeJSON(w, 201, map[string]any{
		"id":          tokenID,
		"name":        input.Name,
		"token":       generated.Plaintext,
		"fingerprint": generated.Fingerprint,
		"scopes":      auth.ParseManagementScopes(scopesJSON),
		"expires_at":  nullableString(expiresAt.String),
		"warning":     "Copy this token now. It cannot be displayed again.",
	})
}

func (s *Server) updateManagementToken(w http.ResponseWriter, r *http.Request) {
	if rejectEnvironmentManagementTokenMutation(w, r) {
		return
	}
	var input struct {
		Name        *string         `json:"name"`
		Description *string         `json:"description"`
		Scopes      *map[string]int `json:"scopes"`
		Enabled     *bool           `json:"enabled"`
		ExpiresAt   *string         `json:"expires_at"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	tokenID := r.PathValue("id")
	current, err := s.scope(r).GetManagementToken(r.Context(), tokenID)
	if errors.Is(err, store.ErrManagementTokenNotFound) {
		adminError(w, 404, "not_found", "API token not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not update API token.")
		return
	}
	update := store.UpdateManagementTokenInput{
		ID:          tokenID,
		Name:        current.Name,
		Description: current.Description,
		Scopes:      current.Scopes,
		Enabled:     current.Enabled,
		ExpiresAt:   current.ExpiresAt,
	}
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if !validManagementTokenName(name) {
			adminError(w, 400, "invalid_name", "Token names must be 1-100 characters.")
			return
		}
		if rejectReservedManagementTokenName(w, name) {
			return
		}
		update.Name = name
	}
	if input.Description != nil {
		if len(*input.Description) > 500 {
			adminError(w, 400, "invalid_description", "Descriptions are limited to 500 characters.")
			return
		}
		update.Description = *input.Description
	}
	if input.Scopes != nil {
		scopesJSON, err := auth.ValidateManagementScopes(*input.Scopes)
		if err != nil {
			adminError(w, 400, "invalid_scopes", err.Error())
			return
		}
		update.Scopes = scopesJSON
	}
	if input.Enabled != nil {
		update.Enabled = *input.Enabled
	}
	if input.ExpiresAt != nil {
		expiresAt, err := parseManagementTokenExpiry(*input.ExpiresAt)
		if err != nil {
			adminError(w, 400, "invalid_expiry", err.Error())
			return
		}
		update.ExpiresAt = expiresAt
	}
	if s.hostedTokens() {
		if input.Scopes != nil {
			if msg := hostedTokenScopeError(*input.Scopes); msg != "" {
				adminError(w, 400, "invalid_scopes", msg)
				return
			}
		}
	}
	// The mutation and cache invalidation publish together so a token whose
	// scopes were just narrowed cannot authorize from a stale cache entry.
	err = s.tokens.InvalidateWith(tokenID, func() error {
		return s.scope(r).UpdateManagementToken(r.Context(), update)
	})
	if errors.Is(err, store.ErrManagementTokenNotFound) {
		adminError(w, 404, "not_found", "API token not found.")
		return
	} else if err != nil {
		if database.IsConstraint(err) {
			adminError(w, 409, "name_conflict", "An API token with that name already exists.")
			return
		}
		adminError(w, 500, "database_error", "Could not update API token.")
		return
	}
	s.recordResourceAudit(r, s.scope(r).AccountID(), store.AuditEvent{
		Event:      "api_token.updated",
		TargetType: "api_token",
		TargetID:   tokenID,
		Metadata:   map[string]string{"name": update.Name},
	})
	updated, err := s.scope(r).GetManagementToken(r.Context(), tokenID)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load API token.")
		return
	}
	writeJSON(w, 200, managementTokenViewFromRow(updated))
}

func (s *Server) rotateManagementToken(w http.ResponseWriter, r *http.Request) {
	if rejectEnvironmentManagementTokenMutation(w, r) {
		return
	}
	tokenID := r.PathValue("id")
	if s.hostedTokens() {
		if _, err := s.scope(r).GetManagementToken(r.Context(), tokenID); errors.Is(err, store.ErrManagementTokenNotFound) {
			adminError(w, 404, "not_found", "API token not found.")
			return
		}
	}
	generated, err := auth.GenerateManagementTokenWithHasher(s.secretHasher)
	if err != nil {
		adminError(w, 500, "internal_error", "Could not rotate API token.")
		return
	}
	err = s.tokens.InvalidateWith(tokenID, func() error {
		found, err := s.scope(r).RotateManagementToken(r.Context(), tokenID, generated.Selector, generated.Hash, generated.Fingerprint)
		if err != nil {
			return err
		}
		if !found {
			return store.ErrManagementTokenNotFound
		}
		return nil
	})
	if errors.Is(err, store.ErrManagementTokenNotFound) {
		adminError(w, 404, "not_found", "API token not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not rotate API token.")
		return
	}
	s.recordResourceAudit(r, s.scope(r).AccountID(), store.AuditEvent{
		Event:      "api_token.rotated",
		TargetType: "api_token",
		TargetID:   tokenID,
	})
	writeJSON(w, 200, map[string]any{"id": tokenID, "token": generated.Plaintext, "fingerprint": generated.Fingerprint, "warning": "Copy this token now. The previous token is already invalid and this one cannot be displayed again."})
}

func (s *Server) deleteManagementToken(w http.ResponseWriter, r *http.Request) {
	if rejectEnvironmentManagementTokenMutation(w, r) {
		return
	}
	tokenID := r.PathValue("id")
	name := ""
	if current, err := s.scope(r).GetManagementToken(r.Context(), tokenID); err == nil {
		name = current.Name
	} else if !errors.Is(err, store.ErrManagementTokenNotFound) {
		adminError(w, 500, "database_error", "Could not delete API token.")
		return
	}
	err := s.tokens.InvalidateWith(tokenID, func() error {
		found, err := s.scope(r).DeleteManagementToken(r.Context(), tokenID)
		if err != nil {
			return err
		}
		if !found {
			return store.ErrManagementTokenNotFound
		}
		return nil
	})
	if errors.Is(err, store.ErrManagementTokenNotFound) {
		adminError(w, 404, "not_found", "API token not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not delete API token.")
		return
	}
	s.recordResourceAudit(r, s.scope(r).AccountID(), store.AuditEvent{
		Event:      "api_token.deleted",
		TargetType: "api_token",
		TargetID:   tokenID,
		Metadata:   map[string]string{"name": name},
	})
	w.WriteHeader(204)
}

func (s *Server) revokeAllManagementTokens(w http.ResponseWriter, r *http.Request) {
	accountID := s.scope(r).AccountID()
	count, err := s.scope(r).RevokeAllManagementTokens(r.Context())
	if err != nil {
		adminError(w, 500, "database_error", "Could not revoke API tokens.")
		return
	}
	// Tenant-scoped: only this account's cached tokens are dropped. A global
	// cache wipe here would evict every other tenant's verified tokens and could
	// fail their concurrent verifications closed.
	s.tokens.InvalidateAccount(accountID)
	s.recordResourceAudit(r, accountID, store.AuditEvent{
		Event:      "api_token.revoked_all",
		TargetType: "api_token",
		Metadata:   map[string]string{"count": strconv.FormatInt(count, 10)},
	})
	writeJSON(w, 200, map[string]any{"revoked": count, "environment_managed_excluded": s.config.AdminAPIKey != ""})
}

func rejectEnvironmentManagementTokenMutation(w http.ResponseWriter, r *http.Request) bool {
	if r.PathValue("id") != bootManagementTokenID {
		return false
	}
	adminError(w, http.StatusConflict, "environment_managed_token", "This token is environment-managed. Change the configured key and restart to rotate; remove TILLER_ADMIN_API_KEY and TILLER_ADMIN_API_KEY_FILE and restart to revoke.")
	return true
}

// managementTokenSelf lets a management token introspect itself: name,
// description, scopes, and expiry. It never exposes a secret and never touches
// sibling tokens. Cookie-authenticated callers have no token principal, so they
// receive 404 (the browser reads the token list instead).
func (s *Server) managementTokenSelf(w http.ResponseWriter, r *http.Request) {
	identity, ok := r.Context().Value(managementTokenKey).(auth.ManagementTokenIdentity)
	if !ok || identity.ID == "" {
		adminError(w, 404, "not_found", "This endpoint is for API token introspection.")
		return
	}
	row, err := s.scope(r).GetManagementToken(r.Context(), identity.ID)
	if errors.Is(err, store.ErrManagementTokenNotFound) {
		adminError(w, 404, "not_found", "API token not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not load API token.")
		return
	}
	writeJSON(w, 200, managementTokenViewFromRow(row))
}
