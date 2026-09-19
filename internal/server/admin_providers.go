package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/store"
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
	rows, err := s.scope(r).ListProviders(r.Context(), store.ProviderFilter{Search: search, Limit: limit, Offset: offset})
	if err != nil {
		adminError(w, 500, "database_error", "Could not list providers.")
		return
	}
	data := make([]providerView, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		data = append(data, providerView{
			ID:                   row.ID,
			Name:                 row.Name,
			Type:                 row.Type,
			BaseURL:              row.BaseURL,
			Enabled:              row.Enabled,
			Protocols:            providers.DecodeProtocols(row.Protocols),
			CredentialConfigured: row.CredentialConfigured,
			AuthState:            row.AuthState,
			LastRefreshAt:        row.LastRefreshAt,
			NextRefreshAt:        row.NextRefreshAt,
			LastRefreshError:     row.LastRefreshError,
			CreatedAt:            row.CreatedAt,
			UpdatedAt:            row.UpdatedAt,
			ModelCount:           row.ModelCount,
			AvailableModelCount:  row.AvailableModelCount,
		})
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
	input.Name = strings.TrimSpace(input.Name)
	// Matches DB CHECK: name=lower(name) AND length 1..63 AND GLOB '[a-z0-9-]*' AND first/last [a-z0-9]
	// Validate the raw input (not a lowercased copy) so an all-caps or
	// mixed-case name surfaces a clear error instead of being silently
	// rewritten. The rules are the storage rules; the name must already
	// comply.
	if input.Name != strings.ToLower(input.Name) {
		adminError(w, 400, "invalid_provider_name", "Provider name must be lowercase: use only lowercase letters, digits, and hyphens.")
		return
	}
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
	sc := s.scope(r)
	baseName := input.Name
	committed := false
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
		err = sc.CreateProvider(r.Context(), store.CreateProviderInput{
			ID:         providerID,
			Name:       candidate,
			Type:       input.Type,
			BaseURL:    input.BaseURL,
			Credential: input.Credential,
			Enabled:    enabled,
			Protocols:  providers.EncodeProtocols(protocols),
		})
		if err == nil {
			committed = true
			break
		}
		if database.IsConstraint(err) {
			continue
		}
		adminError(w, 500, "database_error", "Could not create provider.")
		return
	}
	if !committed {
		adminError(w, 409, "name_conflict", "Provider and virtual group names share one namespace; choose another name.")
		return
	}
	refreshCtx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	refreshErr := s.providers.Refresh(refreshCtx, sc.AccountID(), providerID)
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
	sc := s.scope(r)
	current, err := sc.GetProviderEditable(r.Context(), providerID)
	if errors.Is(err, store.ErrProviderNotFound) {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not update provider.")
		return
	}
	if input.Name != nil && *input.Name != current.Name {
		if !input.ConfirmBreaking {
			adminError(w, 409, "breaking_change_confirmation_required", "Renaming changes every client-facing model ID. Confirm the breaking change.")
			return
		}
		current.Name = strings.TrimSpace(*input.Name)
	}
	if input.BaseURL != nil {
		base := strings.TrimRight(*input.BaseURL, "/")
		if providers.ValidateBaseURL(base) != nil {
			adminError(w, 400, "invalid_base_url", "A valid provider base URL is required.")
			return
		}
		current.BaseURL = base
	}
	if input.Enabled != nil {
		current.Enabled = *input.Enabled
	}
	if len(input.Protocols) > 0 && (current.Type == "generic-openai" || current.Type == "vllm") {
		current.Protocols = providers.EncodeProtocols(input.Protocols)
	}
	err = sc.UpdateProvider(r.Context(), store.UpdateProviderInput{
		ID:        providerID,
		Name:      current.Name,
		BaseURL:   current.BaseURL,
		Enabled:   current.Enabled,
		Protocols: current.Protocols,
	})
	if errors.Is(err, store.ErrProviderNotFound) {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	}
	if err != nil {
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
	sc := s.scope(r)
	providerType, err := sc.ProviderType(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrProviderNotFound) {
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
	found, err := sc.ReplaceProviderCredential(r.Context(), r.PathValue("id"), input.Credential)
	if err != nil {
		adminError(w, 500, "database_error", "Could not replace credential.")
		return
	}
	if !found {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) refreshProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.providers.Refresh(ctx, s.scope(r).AccountID(), r.PathValue("id")); err != nil {
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
	sc := s.scope(r)
	providerType, err := sc.ProviderType(r.Context(), providerID)
	if errors.Is(err, store.ErrProviderNotFound) {
		adminError(w, 404, "not_found", "Provider not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not delete provider.")
		return
	}
	err = sc.DeleteProvider(r.Context(), providerID)
	var inUse *store.ProviderInUseError
	switch {
	case errors.As(err, &inUse):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "provider_in_use",
				"message": "This provider is the last target in one or more virtual model fallback chains. Repoint those chains before deleting.",
				"data": map[string]any{
					"blocked":        inUse.Blocked,
					"referenced":     inUse.Referenced,
					"terminal_count": len(inUse.Blocked),
				},
			},
		})
		return
	case errors.Is(err, store.ErrSingleBindingInUse):
		adminError(w, 409, "single_binding_in_use", "Repoint Single client keys using this provider first.")
		return
	case errors.Is(err, store.ErrProviderNotFound):
		adminError(w, 404, "not_found", "Provider not found.")
		return
	case err != nil:
		adminError(w, 500, "database_error", "Could not delete provider.")
		return
	}
	// Clean up in-memory OAuth state only after the transaction commits.
	if descriptor, ok := providers.Lookup(providerType); ok && descriptor.AuthMode == providers.AuthModeOAuth {
		s.oauthDeviceMu.Lock()
		delete(s.oauthDevices, tenantKey(sc.AccountID(), providerID))
		s.oauthDeviceMu.Unlock()
		s.oauthFlows.Cancel(sc.AccountID(), providerID)
	}
	// Drop the per-provider refresh lock so the map does not grow without
	// bound as providers are created and deleted.
	s.providers.DropProviderLock(sc.AccountID(), providerID)
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
	Origin                   string                           `json:"origin"`
}

// manualModelInput is the admin-supplied shape shared by the add and lookup
// handlers. Empty/nil metadata fields mean "detect".
type manualModelInput struct {
	UpstreamModelID string             `json:"upstream_model_id"`
	DisplayName     string             `json:"display_name"`
	ContextLength   *int64             `json:"context_length"`
	MaxOutputTokens *int64             `json:"max_output_tokens"`
	NativeProtocol  providers.Protocol `json:"native_protocol"`
}

func (in *manualModelInput) normalizeAndValidate(w http.ResponseWriter) bool {
	in.UpstreamModelID = strings.TrimSpace(in.UpstreamModelID)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if in.UpstreamModelID == "" {
		adminError(w, 400, "model_id_required", "An upstream model ID is required.")
		return false
	}
	if len(in.UpstreamModelID) > 255 || strings.Contains(in.UpstreamModelID, "\x00") {
		adminError(w, 400, "invalid_model_id", "The upstream model ID is invalid.")
		return false
	}
	if in.ContextLength != nil && *in.ContextLength <= 0 || in.MaxOutputTokens != nil && *in.MaxOutputTokens <= 0 {
		adminError(w, 400, "invalid_model_metadata", "Model metadata values must be positive.")
		return false
	}
	if in.NativeProtocol != "" && in.NativeProtocol != providers.ProtocolChat && in.NativeProtocol != providers.ProtocolResponses && in.NativeProtocol != providers.ProtocolMessages {
		adminError(w, 400, "invalid_protocol", "Unknown native protocol.")
		return false
	}
	return true
}

func (s *Server) addManualModel(w http.ResponseWriter, r *http.Request) {
	var input manualModelInput
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	if !input.normalizeAndValidate(w) {
		return
	}
	modelID, err := s.providers.AddManualModel(r.Context(), s.scope(r).AccountID(), r.PathValue("id"), providers.ManualModelInput{
		UpstreamModelID: input.UpstreamModelID,
		DisplayName:     input.DisplayName,
		ContextLength:   input.ContextLength,
		MaxOutputTokens: input.MaxOutputTokens,
		NativeProtocol:  input.NativeProtocol,
	})
	switch {
	case errors.Is(err, providers.ErrManualModelExists):
		adminError(w, 409, "model_exists", "That model already exists for this provider.")
		return
	case errors.Is(err, providers.ErrProviderNotFound):
		adminError(w, 404, "not_found", "Provider not found.")
		return
	case err != nil:
		adminError(w, 500, "database_error", "Could not create model.")
		return
	}
	writeJSON(w, 201, map[string]any{"id": modelID})
}

// lookupManualModel resolves metadata for a manual model without persisting it,
// so the admin UI can preview detection results before saving. Live provider
// discovery is tried first, then models.dev gap-fill.
func (s *Server) lookupManualModel(w http.ResponseWriter, r *http.Request) {
	upstreamID := strings.TrimSpace(r.URL.Query().Get("upstream_model_id"))
	if upstreamID == "" || len(upstreamID) > 255 || strings.Contains(upstreamID, "\x00") {
		adminError(w, 400, "model_id_required", "A valid upstream model ID is required.")
		return
	}
	model, err := s.providers.ResolveManualModel(r.Context(), s.scope(r).AccountID(), r.PathValue("id"), upstreamID)
	switch {
	case errors.Is(err, providers.ErrProviderNotFound):
		adminError(w, 404, "not_found", "Provider not found.")
		return
	case err != nil:
		adminError(w, 500, "database_error", "Could not resolve model metadata.")
		return
	}
	writeJSON(w, 200, map[string]any{
		"upstream_model_id": model.ID,
		"display_name":      model.DisplayName,
		"context_length":    positiveIntOrNil(model.ContextLength),
		"max_output_tokens": positiveIntOrNil(model.MaxOutputTokens),
		"native_protocol":   string(model.NativeProtocol),
	})
}

func positiveIntOrNil(value int) any {
	if value <= 0 {
		return nil
	}
	return value
}

func (s *Server) deleteManualModel(w http.ResponseWriter, r *http.Request) {
	err := s.scope(r).DeleteManualModel(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrModelNotFound):
		adminError(w, 404, "not_found", "Model not found.")
		return
	case errors.Is(err, store.ErrModelNotManual):
		adminError(w, 403, "model_not_manual", "Only manually-added models can be deleted here.")
		return
	case errors.Is(err, store.ErrModelInUse):
		adminError(w, 409, "model_in_use", "Repoint clients and virtual models using this model first.")
		return
	case err != nil:
		adminError(w, 500, "database_error", "Could not delete model.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listProviderModels(w http.ResponseWriter, r *http.Request) {
	s.listModelsQuery(w, r, r.PathValue("id"))
}
func (s *Server) listAllModels(w http.ResponseWriter, r *http.Request) {
	s.listModelsQuery(w, r, "")
}
func (s *Server) listModelsQuery(w http.ResponseWriter, r *http.Request, providerID string) {
	limit, offset, search := pagination(r)
	if r.URL.Query().Get("all") == "1" {
		limit = 100000 // return the full catalogue (e.g. for the virtual-model target selector)
		offset = 0
	}
	rows, err := s.scope(r).ListModels(r.Context(), store.ModelFilter{ProviderID: providerID, Search: search, Limit: limit, Offset: offset})
	if err != nil {
		adminError(w, 500, "database_error", "Could not list models.")
		return
	}
	data := make([]modelView, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		v := modelView{
			ID:               row.ID,
			ProviderID:       row.ProviderID,
			ProviderName:     row.ProviderName,
			UpstreamModelID:  row.UpstreamModelID,
			CanonicalModelID: row.CanonicalModelID,
			DisplayName:      row.DisplayName,
			ContextLength:    row.ContextLength,
			MaxOutputTokens:  row.MaxOutputTokens,
			Available:        row.Available,
			FirstSeenAt:      row.FirstSeenAt,
			LastSeenAt:       row.LastSeenAt,
			Origin:           row.Origin,
		}
		if row.NativeProtocol.Valid {
			v.NativeProtocol = providers.Protocol(row.NativeProtocol.String)
		}
		v.SupportsTools = triBoolFromInt(row.SupportsTools)
		v.SupportsVision = triBoolFromInt(row.SupportsVision)
		v.SupportsReasoning = triBoolFromInt(row.SupportsReasoning)
		v.SupportsStructuredOutput = triBoolFromInt(row.SupportsStructuredOutput)
		v.ReasoningCapabilities = decodeReasoningCapabilities(row.ReasoningCapabilities)
		v.InputModalities = decodeModalities(row.InputModalities)
		v.OutputModalities = decodeModalities(row.OutputModalities)
		data = append(data, v)
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) adminHealth(w http.ResponseWriter, r *http.Request) {
	counts, err := s.scope(r).AdminHealth(r.Context())
	if err != nil {
		adminError(w, 500, "database_error", "Could not load health.")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ready", "providers": counts.Providers, "available_models": counts.AvailableModels, "retired_models": counts.RetiredModels, "broken_virtual_models": counts.BrokenVirtualModels})
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
