package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/providers"
)

type providerView struct {
	ID                   string               `json:"id"`
	Name                 string               `json:"name"`
	Type                 string               `json:"type"`
	BaseURL              string               `json:"base_url"`
	Enabled              bool                 `json:"enabled"`
	Protocols            []providers.Protocol `json:"protocols"`
	CredentialConfigured bool                 `json:"credential_configured"`
	AuthState            string               `json:"auth_state"`
	LastRefreshAt        *string              `json:"last_refresh_at"`
	NextRefreshAt        *string              `json:"next_refresh_at"`
	LastRefreshError     *string              `json:"last_refresh_error"`
	CreatedAt            string               `json:"created_at"`
	UpdatedAt            string               `json:"updated_at"`
	ModelCount           int                  `json:"model_count"`
	AvailableModelCount  int                  `json:"available_model_count"`
}

func (s *Server) providerTypes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"data": providers.Descriptors()})
}

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	limit, offset, search := pagination(r)
	pattern := "%" + search + "%"
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT p.id,p.name,p.type,p.base_url,p.enabled,p.protocols,(p.credential_secret IS NOT NULL OR EXISTS(SELECT 1 FROM provider_oauth_tokens o WHERE o.provider_id=p.id)),coalesce(o.auth_state,''),p.last_refresh_at,p.next_refresh_at,p.last_refresh_error,p.created_at,p.updated_at,count(m.id),coalesce(sum(CASE WHEN m.available=1 THEN 1 ELSE 0 END),0) FROM providers p LEFT JOIN provider_models m ON m.provider_id=p.id LEFT JOIN provider_oauth_tokens o ON o.provider_id=p.id WHERE p.name LIKE ? OR p.type LIKE ? GROUP BY p.id ORDER BY p.name LIMIT ? OFFSET ?`, pattern, pattern, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not list providers.")
		return
	}
	defer rows.Close()
	data := []providerView{}
	for rows.Next() {
		var v providerView
		var enabled, configured int
		var raw string
		if err := rows.Scan(&v.ID, &v.Name, &v.Type, &v.BaseURL, &enabled, &raw, &configured, &v.AuthState, &v.LastRefreshAt, &v.NextRefreshAt, &v.LastRefreshError, &v.CreatedAt, &v.UpdatedAt, &v.ModelCount, &v.AvailableModelCount); err != nil {
			adminError(w, 500, "database_error", "Could not list providers.")
			return
		}
		v.Enabled = scanBool(enabled)
		v.CredentialConfigured = scanBool(configured)
		v.Protocols = providers.DecodeProtocols(raw)
		data = append(data, v)
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) createProvider(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name       string               `json:"name"`
		Type       string               `json:"type"`
		BaseURL    string               `json:"base_url"`
		Credential string               `json:"credential"`
		Enabled    *bool                `json:"enabled"`
		Protocols  []providers.Protocol `json:"protocols"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	descriptor, ok := providers.Lookup(input.Type)
	if !ok {
		adminError(w, 400, "invalid_provider_type", "Unknown provider type.")
		return
	}
	if input.Name == "" {
		input.Name = descriptor.Type
		if input.Type == "codex-subscription" {
			input.Name = "codex"
		}
	}
	input.Name = strings.ToLower(strings.TrimSpace(input.Name))
	// Matches DB CHECK: name=lower(name) AND length 1..63 AND GLOB '[a-z0-9-]*' AND first/last [a-z0-9]
	if len(input.Name) < 1 || len(input.Name) > 63 {
		adminError(w, 400, "invalid_provider_name", "Provider name must be 1-63 lowercase alphanumerics/hyphens.")
		return
	}
	if input.Name[0] == '-' || input.Name[len(input.Name)-1] == '-' {
		adminError(w, 400, "invalid_provider_name", "Provider name must start and end with alphanumeric.")
		return
	}
	for _, ch := range input.Name {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			adminError(w, 400, "invalid_provider_name", "Provider name may only contain lowercase letters, digits, and hyphens.")
			return
		}
	}
	if input.BaseURL == "" {
		input.BaseURL = descriptor.DefaultBaseURL
	}
	input.BaseURL = strings.TrimRight(input.BaseURL, "/")
	if input.BaseURL == "" || providers.ValidateBaseURL(input.BaseURL) != nil {
		adminError(w, 400, "invalid_base_url", "A valid provider base URL is required.")
		return
	}
	if descriptor.CredentialNeeded && input.Credential == "" {
		adminError(w, 400, "credential_required", "This provider requires an API credential.")
		return
	}
	if descriptor.AuthMode == providers.AuthModeOAuth && input.Credential != "" {
		adminError(w, 400, "oauth_credential_not_allowed", "This provider must be connected through OAuth.")
		return
	}
	protocols := descriptor.Protocols
	if (input.Type == "generic-openai" || input.Type == "vllm") && len(input.Protocols) > 0 {
		protocols = input.Protocols
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	providerID, err := id.New()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not create provider.")
		return
	}
	now := database.Now()
	baseName := input.Name
	var committed bool
	var lastErr error
	for attempt := 0; attempt < 100; attempt++ {
		candidate := baseName
		if attempt > 0 {
			suffix := fmt.Sprintf("-%d", attempt+1)
			maxBase := 63 - len(suffix)
			b := baseName
			if len(b) > maxBase {
				b = b[:maxBase]
			}
			b = strings.TrimRight(b, "-")
			if b == "" {
				b = baseName[:1]
			}
			candidate = b + suffix
		}
		input.Name = candidate
		tx, txErr := s.db.SQL.BeginTx(r.Context(), nil)
		if txErr != nil {
			adminError(w, 500, "database_error", "Could not create provider.")
			return
		}
		func() {
			defer tx.Rollback()
			if _, lastErr = tx.ExecContext(r.Context(), `INSERT INTO namespaces(name,kind,entity_id) VALUES(?,'real',?)`, input.Name, providerID); lastErr == nil {
				_, lastErr = tx.ExecContext(r.Context(), `INSERT INTO providers(id,name,type,base_url,credential_secret,enabled,protocols,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, providerID, input.Name, input.Type, input.BaseURL, nullableString(input.Credential), boolInt(enabled), providers.EncodeProtocols(protocols), now, now)
			}
			if lastErr == nil {
				_, lastErr = tx.ExecContext(r.Context(), `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at) SELECT id,'real',?,0,? FROM client_keys`, providerID, now)
			}
			if lastErr == nil {
				lastErr = tx.Commit()
			}
			if lastErr == nil {
				committed = true
			}
		}()
		if committed {
			break
		}
		if lastErr != nil && database.IsConstraint(lastErr) {
			continue
		}
		if lastErr != nil {
			if database.IsConstraint(lastErr) {
				adminError(w, 409, "name_conflict", "Provider and virtual group names share one namespace; choose another name.")
			} else {
				adminError(w, 500, "database_error", "Could not create provider.")
			}
			return
		}
	}
	if !committed {
		adminError(w, 409, "name_conflict", "Provider and virtual group names share one namespace; choose another name.")
		return
	}
	refreshCtx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	refreshErr := s.providers.Refresh(refreshCtx, providerID)
	status := http.StatusCreated
	message := ""
	if refreshErr != nil {
		message = "Provider was saved, but initial discovery failed."
	}
	writeJSON(w, status, map[string]any{"id": providerID, "name": input.Name, "credential_configured": input.Credential != "", "refresh_error": message})
}

func (s *Server) updateProvider(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	var input struct {
		Name            *string              `json:"name"`
		BaseURL         *string              `json:"base_url"`
		Enabled         *bool                `json:"enabled"`
		Protocols       []providers.Protocol `json:"protocols"`
		ConfirmBreaking bool                 `json:"confirm_breaking_change"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not update provider.")
		return
	}
	defer tx.Rollback()
	var oldName, providerType string
	if err = tx.QueryRowContext(r.Context(), `SELECT name,type FROM providers WHERE id=?`, providerID).Scan(&oldName, &providerType); err == sql.ErrNoRows {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not update provider.")
		return
	}
	if input.Name != nil && *input.Name != oldName {
		if !input.ConfirmBreaking {
			adminError(w, 409, "breaking_change_confirmation_required", "Renaming changes every client-facing model ID. Confirm the breaking change.")
			return
		}
		name := strings.TrimSpace(*input.Name)
		_, err = tx.ExecContext(r.Context(), `UPDATE namespaces SET name=? WHERE entity_id=? AND kind='real'`, name, providerID)
	}
	if err == nil && input.BaseURL != nil {
		base := strings.TrimRight(*input.BaseURL, "/")
		if providers.ValidateBaseURL(base) != nil {
			adminError(w, 400, "invalid_base_url", "A valid provider base URL is required.")
			return
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE providers SET base_url=?,updated_at=? WHERE id=?`, base, database.Now(), providerID)
	}
	if err == nil && input.Enabled != nil {
		_, err = tx.ExecContext(r.Context(), `UPDATE providers SET enabled=?,updated_at=? WHERE id=?`, boolInt(*input.Enabled), database.Now(), providerID)
	}
	if err == nil && len(input.Protocols) > 0 && (providerType == "generic-openai" || providerType == "vllm") {
		_, err = tx.ExecContext(r.Context(), `UPDATE providers SET protocols=?,updated_at=? WHERE id=?`, providers.EncodeProtocols(input.Protocols), database.Now(), providerID)
	}
	if err != nil || tx.Commit() != nil {
		if database.IsConstraint(err) {
			adminError(w, 409, "name_conflict", "That provider-group name is already in use.")
		} else {
			adminError(w, 500, "database_error", "Could not update provider.")
		}
		return
	}
	w.WriteHeader(204)
}

func (s *Server) replaceProviderCredential(w http.ResponseWriter, r *http.Request) {
	var providerType string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT type FROM providers WHERE id=?`, r.PathValue("id")).Scan(&providerType); err == sql.ErrNoRows {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not load provider.")
		return
	}
	if descriptor, ok := providers.Lookup(providerType); ok && descriptor.AuthMode == providers.AuthModeOAuth {
		adminError(w, 400, "oauth_credential_not_allowed", "This provider must be connected through OAuth.")
		return
	}
	var input struct {
		Credential string `json:"credential"`
	}
	if decodeJSON(w, r, &input) != nil || input.Credential == "" {
		adminError(w, 400, "credential_required", "A non-empty credential is required.")
		return
	}
	result, err := s.db.SQL.ExecContext(r.Context(), `UPDATE providers SET credential_secret=?,updated_at=? WHERE id=?`, input.Credential, database.Now(), r.PathValue("id"))
	if err != nil {
		adminError(w, 500, "database_error", "Could not replace credential.")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) refreshProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.providers.Refresh(ctx, r.PathValue("id")); err != nil {
		adminError(w, 502, "refresh_failed", "Refresh failed; the previous catalogue and permissions were preserved.")
		return
	}
	// A manual catalogue refresh also picks up fresh models.dev metadata in the
	// background (best-effort; never blocks or fails the refresh response).
	if s.config.ModelsDevEnabled {
		s.providers.Registry().RefreshModelsDevIfStale(context.Background(), filepath.Join(s.config.DataDir, providers.ModelsDevCacheFile()))
	}
	writeJSON(w, 200, map[string]any{"status": "refreshed"})
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	var providerType string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT type FROM providers WHERE id=?`, providerID).Scan(&providerType); err == sql.ErrNoRows {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not delete provider.")
		return
	}
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not delete provider.")
		return
	}
	defer tx.Rollback()
	var refs int
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM client_single_bindings b JOIN client_keys c ON c.id = b.client_key_id JOIN provider_models m ON m.id = b.real_model_id WHERE m.provider_id=? AND c.key_type='single'`, providerID).Scan(&refs); err != nil {
		adminError(w, 500, "database_error", "Could not delete provider.")
		return
	}
	if refs > 0 {
		adminError(w, 409, "single_binding_in_use", "Repoint Single client keys using this provider first.")
		return
	}
	// Catalogue-type client keys may carry a stale client_single_bindings row
	// from a prior Single-key configuration. Such rows are inert (catalogue
	// keys are resolved through client_model_permissions, not the binding),
	// but their ON DELETE RESTRICT foreign key would otherwise block this
	// delete. Drop them explicitly so the spec's "Single keys" intent wins.
	if _, err = tx.ExecContext(r.Context(), `DELETE FROM client_single_bindings WHERE real_model_id IN (SELECT id FROM provider_models WHERE provider_id=?) AND client_key_id IN (SELECT id FROM client_keys WHERE key_type='catalogue')`, providerID); err != nil {
		adminError(w, 500, "database_error", "Could not delete provider.")
		return
	}
	// virtual_model_targets is the functional source of truth. This includes
	// references in every ordered position, not only the compatibility primary
	// columns on virtual_models.
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id WHERE m.provider_id=?`, providerID).Scan(&refs); err != nil {
		adminError(w, 500, "database_error", "Could not delete provider.")
		return
	}
	if refs > 0 {
		adminError(w, 409, "provider_in_use", "Repoint or delete dependent virtual models first.")
		return
	}
	var name string
	if err = tx.QueryRowContext(r.Context(), `SELECT name FROM providers WHERE id=?`, providerID).Scan(&name); err == sql.ErrNoRows {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	}
	_, err = tx.ExecContext(r.Context(), `DELETE FROM client_model_permissions WHERE model_kind='real' AND model_id IN (SELECT id FROM provider_models WHERE provider_id=?)`, providerID)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `DELETE FROM client_group_defaults WHERE group_kind='real' AND group_id=?`, providerID)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `DELETE FROM client_single_bindings WHERE client_key_id IN (SELECT b.client_key_id FROM client_single_bindings b JOIN client_keys c ON c.id=b.client_key_id JOIN provider_models m ON m.id=b.real_model_id WHERE c.key_type='catalogue' AND m.provider_id=?)`, providerID)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `DELETE FROM provider_models WHERE provider_id=?`, providerID)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `DELETE FROM providers WHERE id=?`, providerID)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `DELETE FROM namespaces WHERE entity_id=? AND kind='real'`, providerID)
	}
	// Delete absorbs disconnect: clean up OAuth material (token row) inside
	// the transaction, after all reference checks pass. A rejected delete
	// (409) rolls back and leaves credentials untouched.
	if err == nil {
		if descriptor, ok := providers.Lookup(providerType); ok && descriptor.AuthMode == providers.AuthModeOAuth {
			_, err = tx.ExecContext(r.Context(), `DELETE FROM provider_oauth_tokens WHERE provider_id=?`, providerID)
		}
	}
	if err != nil || tx.Commit() != nil {
		adminError(w, 500, "database_error", "Could not delete provider.")
		return
	}
	// Clean up in-memory OAuth state only after the transaction commits.
	if descriptor, ok := providers.Lookup(providerType); ok && descriptor.AuthMode == providers.AuthModeOAuth {
		s.oauthDeviceMu.Lock()
		delete(s.oauthDevices, providerID)
		s.oauthDeviceMu.Unlock()
		s.oauthFlows.Cancel(providerID)
	}
	w.WriteHeader(204)
}

type modelView struct {
	ID                       string                           `json:"id"`
	ProviderID               string                           `json:"provider_id"`
	ProviderName             string                           `json:"provider_name"`
	UpstreamModelID          string                           `json:"upstream_model_id"`
	CanonicalModelID         string                           `json:"canonical_model_id"`
	DisplayName              string                           `json:"display_name"`
	ContextLength            *int64                           `json:"context_length"`
	MaxOutputTokens          *int64                           `json:"max_output_tokens"`
	NativeProtocol           providers.Protocol               `json:"native_protocol,omitempty"`
	SupportsTools            *bool                            `json:"supports_tools"`
	SupportsVision           *bool                            `json:"supports_vision"`
	SupportsReasoning        *bool                            `json:"supports_reasoning"`
	SupportsStructuredOutput *bool                            `json:"supports_structured_output"`
	ReasoningCapabilities    *providers.ReasoningCapabilities `json:"reasoning_capabilities,omitempty"`
	InputModalities          []string                         `json:"input_modalities,omitempty"`
	OutputModalities         []string                         `json:"output_modalities,omitempty"`
	Available                bool                             `json:"available"`
	FirstSeenAt              string                           `json:"first_seen_at"`
	LastSeenAt               string                           `json:"last_seen_at"`
}

func (s *Server) listProviderModels(w http.ResponseWriter, r *http.Request) {
	s.listModelsQuery(w, r, `m.provider_id=?`, []any{r.PathValue("id")})
}
func (s *Server) listAllModels(w http.ResponseWriter, r *http.Request) {
	s.listModelsQuery(w, r, `1=1`, nil)
}
func (s *Server) listModelsQuery(w http.ResponseWriter, r *http.Request, where string, args []any) {
	limit, offset, search := pagination(r)
	if r.URL.Query().Get("all") == "1" {
		limit = 100000 // return the full catalogue (e.g. for the virtual-model target selector)
		offset = 0
	}
	query := `SELECT m.id,m.provider_id,p.name,m.upstream_model_id,p.name||'/'||m.upstream_model_id,m.display_name,m.context_length,m.max_output_tokens,m.native_protocol,m.supports_tools,m.supports_vision,m.supports_reasoning,m.supports_structured_output,m.reasoning_capabilities,m.input_modalities,m.output_modalities,m.available,m.first_seen_at,m.last_seen_at FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE ` + where + ` AND (m.upstream_model_id LIKE ? OR p.name LIKE ?) ORDER BY p.name,m.upstream_model_id LIMIT ? OFFSET ?`
	pattern := "%" + search + "%"
	args = append(args, pattern, pattern, limit, offset)
	rows, err := s.db.SQL.QueryContext(r.Context(), query, args...)
	if err != nil {
		adminError(w, 500, "database_error", "Could not list models.")
		return
	}
	defer rows.Close()
	data := []modelView{}
	for rows.Next() {
		var v modelView
		var available int
		var nativeProtocol sql.NullString
		var tools, vision, reasoning, structured sql.NullInt64
		var reasoningCaps sql.NullString
		var inputMod, outputMod sql.NullString
		if rows.Scan(&v.ID, &v.ProviderID, &v.ProviderName, &v.UpstreamModelID, &v.CanonicalModelID, &v.DisplayName, &v.ContextLength, &v.MaxOutputTokens, &nativeProtocol, &tools, &vision, &reasoning, &structured, &reasoningCaps, &inputMod, &outputMod, &available, &v.FirstSeenAt, &v.LastSeenAt) != nil {
			adminError(w, 500, "database_error", "Could not list models.")
			return
		}
		if nativeProtocol.Valid {
			v.NativeProtocol = providers.Protocol(nativeProtocol.String)
		}
		v.SupportsTools = triBoolFromInt(tools)
		v.SupportsVision = triBoolFromInt(vision)
		v.SupportsReasoning = triBoolFromInt(reasoning)
		v.SupportsStructuredOutput = triBoolFromInt(structured)
		v.ReasoningCapabilities = decodeReasoningCapabilities(reasoningCaps)
		v.InputModalities = decodeModalities(inputMod)
		v.OutputModalities = decodeModalities(outputMod)
		v.Available = scanBool(available)
		data = append(data, v)
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) adminHealth(w http.ResponseWriter, r *http.Request) {
	var providersCount, available, retired, broken int
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT count(*) FROM providers`).Scan(&providersCount)
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT count(*) FROM provider_models WHERE available=1`).Scan(&available)
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT count(*) FROM provider_models WHERE available=0`).Scan(&retired)
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT count(*) FROM virtual_models v WHERE NOT EXISTS (SELECT 1 FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=v.id AND t.enabled=1 AND m.available=1 AND p.enabled=1)`).Scan(&broken)
	writeJSON(w, 200, map[string]any{"status": "ready", "providers": providersCount, "available_models": available, "retired_models": retired, "broken_virtual_models": broken})
}

// decodeReasoningCapabilities decodes a stored JSON reasoning_capabilities
// column into a *ReasoningCapabilities. Returns nil when the column is NULL
// or unreadable (unknown).
func decodeReasoningCapabilities(v sql.NullString) *providers.ReasoningCapabilities {
	if !v.Valid || v.String == "" {
		return nil
	}
	var rc *providers.ReasoningCapabilities
	if err := json.Unmarshal([]byte(v.String), &rc); err != nil {
		return nil
	}
	return rc
}

// triBoolFromInt converts a nullable tri-state capability column (NULL/0/1)
// into a *bool (nil = unknown).
func triBoolFromInt(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

// decodeModalities decodes a stored JSON array of modality strings; nil when
// the column is NULL or empty.
func decodeModalities(v sql.NullString) []string {
	if !v.Valid || v.String == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return nil
	}
	return out
}

// triStateANDBool computes the conservative AND of tri-state capability flags:
// any false -> false; else any nil -> nil (unknown); else true. An empty input
// yields nil (unknown).
func triStateANDBool(flags []*bool) *bool {
	if len(flags) == 0 {
		return nil
	}
	hasUnknown := false
	for _, f := range flags {
		if f == nil {
			hasUnknown = true
			continue
		}
		if !*f {
			b := false
			return &b
		}
	}
	if hasUnknown {
		return nil
	}
	b := true
	return &b
}
