package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/store"
)

// generateClientKey generates a client key using the server's injected
// hasher so tests can override the KDF cost.
func (s *Server) generateClientKey() (auth.GeneratedKey, error) {
	return auth.GenerateKeyWithHasher(s.secretHasher)
}

var clientKeyGroupPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,62}$`)

func normalizeClientKeyGroup(group string) string {
	group = strings.TrimSpace(group)
	if group == "" {
		return "default"
	}
	return group
}

type clientKeyView struct {
	ID                    string  `json:"id"`
	Name                  string  `json:"name"`
	Description           string  `json:"description"`
	Group                 string  `json:"group"`
	Fingerprint           string  `json:"fingerprint"`
	Enabled               bool    `json:"enabled"`
	LoggingEnabled        bool    `json:"logging_enabled"`
	RetentionDays         int     `json:"retention_days"`
	CreatedAt             string  `json:"created_at"`
	RotatedAt             *string `json:"rotated_at"`
	UpdatedAt             string  `json:"updated_at"`
	Type                  string  `json:"type"`
	SingleModelName       string  `json:"single_model_name,omitempty"`
	SingleTargetType      string  `json:"single_target_type,omitempty"`
	SingleTargetID        string  `json:"single_target_id,omitempty"`
	SingleTargetCanonical string  `json:"single_target_canonical,omitempty"`
	SingleTargetAvailable bool    `json:"single_target_available"`
}

var clientModelNamePattern = regexp.MustCompile(`^[A-Za-z0-9._~-](?:[A-Za-z0-9._~/-]{0,253}[A-Za-z0-9._~-])?$`)

func validClientModelName(name string) bool {
	return len(name) >= 1 && len(name) <= 255 && !strings.Contains(name, "//") && clientModelNamePattern.MatchString(name)
}

func (s *Server) listClientKeys(w http.ResponseWriter, r *http.Request) {
	limit, offset, search := pagination(r)
	groupFilter := strings.TrimSpace(r.URL.Query().Get("group"))
	rows, err := s.scope(r).ListClientKeys(r.Context(), store.ClientKeyFilter{Search: search, Group: groupFilter, Limit: limit, Offset: offset})
	if err != nil {
		adminError(w, 500, "database_error", "Could not list client keys.")
		return
	}
	data := make([]clientKeyView, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		v := clientKeyView{
			ID:                    row.ID,
			Name:                  row.Name,
			Description:           row.Description,
			Group:                 row.Group,
			Fingerprint:           row.Fingerprint,
			Enabled:               row.Enabled,
			LoggingEnabled:        row.LoggingEnabled,
			RetentionDays:         row.RetentionDays,
			CreatedAt:             row.CreatedAt,
			UpdatedAt:             row.UpdatedAt,
			Type:                  row.Type,
			SingleModelName:       row.SingleModelName,
			SingleTargetType:      row.SingleTargetType,
			SingleTargetID:        row.SingleTargetID,
			SingleTargetCanonical: row.SingleTargetCanonical,
			SingleTargetAvailable: row.SingleTargetAvailable,
		}
		if row.RotatedAt.Valid {
			rotated := row.RotatedAt.String
			v.RotatedAt = &rotated
		}
		data = append(data, v)
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) createClientKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name             string `json:"name"`
		Description      string `json:"description"`
		Group            string `json:"group"`
		Type             string `json:"type"`
		SingleModelName  string `json:"single_model_name"`
		SingleTargetType string `json:"single_target_type"`
		SingleTargetID   string `json:"single_target_id"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	group := normalizeClientKeyGroup(input.Group)
	if !clientKeyGroupPattern.MatchString(group) {
		adminError(w, 400, "invalid_group", "Group names must be 1-63 characters using letters, digits, spaces, dots, dashes, or underscores.")
		return
	}
	if input.Type == "" {
		input.Type = "catalogue"
	}
	if input.Type != "catalogue" && input.Type != "single" {
		adminError(w, 400, "invalid_client_type", "Client key type must be catalogue or single.")
		return
	}
	if input.Type == "single" {
		if input.SingleModelName == "" {
			input.SingleModelName = "main"
		}
		if !validClientModelName(input.SingleModelName) {
			adminError(w, 400, "invalid_model_name", "Client-facing model names must use 1-255 model-safe characters.")
			return
		}
		if input.SingleTargetID == "" {
			adminError(w, 400, "target_required", "A Single client key requires a target.")
			return
		}
	}
	generated, err := s.generateClientKey()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not create client key.")
		return
	}
	clientID, err := id.New()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not generate client key.")
		return
	}
	loggingEnabled, retentionDays, err := s.scope(r).GetLoggingDefaults(r.Context())
	if err != nil {
		adminError(w, 500, "database_error", "Could not create client key.")
		return
	}
	err = s.scope(r).CreateClientKey(r.Context(), store.CreateClientKeyInput{
		ID:               clientID,
		Name:             input.Name,
		Description:      input.Description,
		Group:            group,
		Selector:         generated.Selector,
		Hash:             generated.Hash,
		Fingerprint:      generated.Fingerprint,
		Type:             input.Type,
		LoggingEnabled:   loggingEnabled,
		RetentionDays:    retentionDays,
		SingleModelName:  input.SingleModelName,
		SingleTargetType: input.SingleTargetType,
		SingleTargetID:   input.SingleTargetID,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrSingleTargetNotFound):
			adminError(w, 400, "invalid_target", "The selected Single-key target does not exist.")
		case database.IsConstraint(err):
			adminError(w, 409, "name_conflict", "A client key with that name already exists.")
		default:
			adminError(w, 500, "database_error", "Could not create client key.")
		}
		return
	}
	s.clients.Invalidate(clientID)
	writeJSON(w, 201, map[string]any{"id": clientID, "name": input.Name, "type": input.Type, "secret": generated.Plaintext, "fingerprint": generated.Fingerprint, "warning": "Copy this key now. It cannot be displayed again."})
	s.notifyAdminEvent(s.scope(r).AccountID(), eventClientKeyCreated, fmt.Sprintf("Client: %s\nType: %s", input.Name, input.Type))
}

func (s *Server) updateClientKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name                   *string `json:"name"`
		Description            *string `json:"description"`
		Group                  *string `json:"group"`
		Enabled                *bool   `json:"enabled"`
		LoggingEnabled         *bool   `json:"logging_enabled"`
		RetentionDays          *int    `json:"retention_days"`
		Type                   *string `json:"type"`
		SingleModelName        *string `json:"single_model_name"`
		SingleTargetType       *string `json:"single_target_type"`
		SingleTargetID         *string `json:"single_target_id"`
		ConfirmModelNameChange bool    `json:"confirm_model_name_change"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	if input.RetentionDays != nil && *input.RetentionDays < 1 {
		adminError(w, 400, "invalid_retention", "Retention must be at least 1 day.")
		return
	}
	clientID := r.PathValue("id")
	sc := s.scope(r)
	current, err := sc.GetClientKeyEditable(r.Context(), clientID)
	if errors.Is(err, store.ErrClientKeyNotFound) {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not update client key.")
		return
	}
	name, description, keyGroup, keyType := current.Name, current.Description, current.Group, current.Type
	enabled, loggingEnabled, retentionDays := current.Enabled, current.LoggingEnabled, current.RetentionDays
	if input.Name != nil {
		name = *input.Name
	}
	if input.Description != nil {
		description = *input.Description
	}
	if input.Group != nil {
		keyGroup = normalizeClientKeyGroup(*input.Group)
		if !clientKeyGroupPattern.MatchString(keyGroup) {
			adminError(w, 400, "invalid_group", "Group names must be 1-63 characters using letters, digits, spaces, dots, dashes, or underscores.")
			return
		}
	}
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	if input.LoggingEnabled != nil {
		loggingEnabled = *input.LoggingEnabled
	}
	if input.RetentionDays != nil {
		retentionDays = *input.RetentionDays
	}
	oldType := keyType
	if input.Type != nil {
		keyType = *input.Type
	}
	if keyType != "catalogue" && keyType != "single" {
		adminError(w, 400, "invalid_client_type", "Client key type must be catalogue or single.")
		return
	}
	binding, bindFound, err := sc.GetSingleBinding(r.Context(), clientID)
	if err != nil {
		adminError(w, 500, "database_error", "Could not update client key.")
		return
	}
	var oldModelName, targetType, targetID string
	if bindFound {
		oldModelName = binding.ModelName
		if binding.RealModelID.Valid {
			targetType, targetID = "real", binding.RealModelID.String
		} else if binding.VirtualModelID.Valid {
			targetType, targetID = "virtual", binding.VirtualModelID.String
		}
	}
	modelName := oldModelName
	if !bindFound {
		modelName = "main"
	}
	if input.SingleModelName != nil {
		modelName = *input.SingleModelName
	}
	if (input.SingleTargetType == nil) != (input.SingleTargetID == nil) {
		adminError(w, 400, "invalid_target", "Target type and target ID must be supplied together.")
		return
	}
	if input.SingleTargetType != nil {
		targetType, targetID = *input.SingleTargetType, *input.SingleTargetID
	}
	bindingSupplied := input.SingleModelName != nil || input.SingleTargetID != nil
	writeBinding := false
	if keyType == "single" || bindingSupplied {
		if !validClientModelName(modelName) {
			adminError(w, 400, "invalid_model_name", "Client-facing model names must use 1-255 model-safe characters.")
			return
		}
		if oldType == "single" && bindFound && modelName != oldModelName && !input.ConfirmModelNameChange {
			adminError(w, 409, "breaking_change_confirmation_required", "Changing the client-facing model name may require client reconfiguration. Confirm the breaking change.")
			return
		}
		writeBinding = true
	}
	err = sc.UpdateClientKey(r.Context(), store.UpdateClientKeyInput{
		ID:             clientID,
		Name:           name,
		Description:    description,
		Group:          keyGroup,
		Type:           keyType,
		Enabled:        enabled,
		LoggingEnabled: loggingEnabled,
		RetentionDays:  retentionDays,
		WriteBinding:   writeBinding,
		ModelName:      modelName,
		TargetType:     targetType,
		TargetID:       targetID,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrSingleTargetNotFound):
			adminError(w, 400, "invalid_target", "The selected Single-key target does not exist.")
		case errors.Is(err, store.ErrClientKeyNotFound):
			adminError(w, 404, "not_found", "Client key not found.")
		case database.IsConstraint(err):
			adminError(w, 409, "name_conflict", "A client key with that name already exists.")
		default:
			adminError(w, 500, "database_error", "Could not update client key.")
		}
		return
	}
	s.clients.Invalidate(clientID)
	w.WriteHeader(204)
}

func (s *Server) rotateClientKey(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	generated, err := s.generateClientKey()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not rotate client key.")
		return
	}
	found, err := s.scope(r).RotateClientKey(r.Context(), clientID, generated.Selector, generated.Hash, generated.Fingerprint)
	if err != nil {
		adminError(w, 500, "database_error", "Could not rotate client key.")
		return
	}
	if !found {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	s.clients.Invalidate(clientID)
	writeJSON(w, 200, map[string]any{"id": clientID, "secret": generated.Plaintext, "fingerprint": generated.Fingerprint, "warning": "Copy this key now. The previous key is already invalid and this one cannot be displayed again."})
}

func (s *Server) deleteClientKey(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	name, err := s.scope(r).ClientKeyName(r.Context(), clientID)
	if errors.Is(err, store.ErrClientKeyNotFound) {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not delete client key.")
		return
	}
	found, err := s.scope(r).DeleteClientKey(r.Context(), clientID)
	if err != nil {
		adminError(w, 500, "database_error", "Could not delete client key.")
		return
	}
	if !found {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	s.clients.Invalidate(clientID)
	w.WriteHeader(204)
	s.notifyAdminEvent(s.scope(r).AccountID(), eventClientKeyDeleted, fmt.Sprintf("Client: %s", name))
}

type permissionGroup struct {
	Kind             string            `json:"kind"`
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	NewModelsEnabled bool              `json:"new_models_enabled"`
	Models           []permissionModel `json:"models"`
}
type permissionModel struct {
	Kind             string `json:"kind"`
	ID               string `json:"id"`
	CanonicalModelID string `json:"canonical_model_id"`
	Enabled          bool   `json:"enabled"`
	Available        bool   `json:"available"`
}

func (s *Server) getPermissions(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	groups, err := s.scope(r).ListPermissions(r.Context(), clientID)
	if errors.Is(err, store.ErrClientKeyNotFound) {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not load permissions.")
		return
	}
	view := make([]permissionGroup, 0, len(groups))
	for _, g := range groups {
		pg := permissionGroup{Kind: g.Kind, ID: g.ID, Name: g.Name, NewModelsEnabled: g.NewModelsEnabled, Models: make([]permissionModel, 0, len(g.Models))}
		for _, m := range g.Models {
			pg.Models = append(pg.Models, permissionModel{Kind: m.Kind, ID: m.ID, CanonicalModelID: m.CanonicalModelID, Enabled: m.Enabled, Available: m.Available})
		}
		view = append(view, pg)
	}
	writeJSON(w, 200, map[string]any{"client_key_id": clientID, "groups": view, "feeder_explanation": "Controls whether models discovered or created in future are enabled for this client. Changing it never alters existing model permissions."})
}

func (s *Server) updatePermissions(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Defaults []struct {
			Kind    string `json:"kind"`
			GroupID string `json:"group_id"`
			Enabled bool   `json:"enabled"`
		} `json:"defaults"`
		Permissions []struct {
			Kind    string `json:"kind"`
			ModelID string `json:"model_id"`
			Enabled bool   `json:"enabled"`
		} `json:"permissions"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	clientID := r.PathValue("id")
	for _, d := range input.Defaults {
		if d.Kind != "real" && d.Kind != "virtual" {
			adminError(w, 400, "invalid_permission", "Invalid group kind.")
			return
		}
	}
	for _, p := range input.Permissions {
		if p.Kind != "real" && p.Kind != "virtual" {
			adminError(w, 400, "invalid_permission", "Invalid model kind.")
			return
		}
	}
	defaults := make([]store.PermissionDefaultUpdate, 0, len(input.Defaults))
	for _, d := range input.Defaults {
		defaults = append(defaults, store.PermissionDefaultUpdate{Kind: d.Kind, GroupID: d.GroupID, Enabled: d.Enabled})
	}
	permissions := make([]store.PermissionModelUpdate, 0, len(input.Permissions))
	for _, p := range input.Permissions {
		permissions = append(permissions, store.PermissionModelUpdate{Kind: p.Kind, ModelID: p.ModelID, Enabled: p.Enabled})
	}
	err := s.scope(r).UpdatePermissions(r.Context(), clientID, defaults, permissions)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrClientKeyNotFound):
			adminError(w, 404, "not_found", "Client key not found.")
		case errors.Is(err, store.ErrPermissionGroupNotFound):
			adminError(w, 400, "invalid_permission", "Unknown permission group.")
		case errors.Is(err, store.ErrPermissionModelNotFound):
			adminError(w, 400, "invalid_permission", "Unknown model permission.")
		default:
			adminError(w, 500, "database_error", "Could not update permissions.")
		}
		return
	}
	s.clients.Invalidate(clientID)
	w.WriteHeader(204)
}
