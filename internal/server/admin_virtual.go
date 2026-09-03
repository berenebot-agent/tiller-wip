package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
)

type virtualTargetInput struct {
	ProviderModelID string `json:"provider_model_id"`
	Enabled         bool   `json:"enabled"`
}

func validateVirtualTargets(mode string, targets []virtualTargetInput) error {
	if mode != "fixed" && mode != "ordered_fallback" {
		return fmt.Errorf("routing_mode must be fixed or ordered_fallback")
	}
	if len(targets) == 0 {
		return fmt.Errorf("at least one target is required")
	}
	seen, active := map[string]bool{}, 0
	for _, target := range targets {
		if target.ProviderModelID == "" {
			return fmt.Errorf("every target needs provider_model_id")
		}
		if seen[target.ProviderModelID] {
			return fmt.Errorf("duplicate target model")
		}
		seen[target.ProviderModelID] = true
		if target.Enabled {
			active++
		}
	}
	if mode == "fixed" && active != 1 {
		return fmt.Errorf("Fixed mode requires exactly one enabled target")
	}
	return nil
}

func replaceVirtualTargets(r *http.Request, tx *sql.Tx, virtualID string, targets []virtualTargetInput, now string) error {
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM virtual_model_targets WHERE virtual_model_id=?`, virtualID); err != nil {
		return err
	}
	for i, target := range targets {
		var exists int
		if err := tx.QueryRowContext(r.Context(), `SELECT count(*) FROM provider_models WHERE id=?`, target.ProviderModelID).Scan(&exists); err != nil || exists != 1 {
			return fmt.Errorf("invalid target model")
		}
		targetID, err := id.New()
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(r.Context(), `INSERT INTO virtual_model_targets(id,virtual_model_id,provider_model_id,position,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, targetID, virtualID, target.ProviderModelID, i+1, boolInt(target.Enabled), now, now); err != nil {
			return err
		}
	}
	return nil
}

type virtualGroupView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
	ModelCount int    `json:"model_count"`
}

func (s *Server) listVirtualGroups(w http.ResponseWriter, r *http.Request) {
	limit, offset, search := pagination(r)
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT g.id,g.name,g.created_at,g.updated_at,count(v.id) FROM virtual_provider_groups g LEFT JOIN virtual_models v ON v.virtual_group_id=g.id WHERE g.name LIKE ? GROUP BY g.id ORDER BY g.name LIMIT ? OFFSET ?`, "%"+search+"%", limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not list virtual groups.")
		return
	}
	defer rows.Close()
	data := []virtualGroupView{}
	for rows.Next() {
		var v virtualGroupView
		if rows.Scan(&v.ID, &v.Name, &v.CreatedAt, &v.UpdatedAt, &v.ModelCount) != nil {
			adminError(w, 500, "database_error", "Could not list virtual groups.")
			return
		}
		data = append(data, v)
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) createVirtualGroup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	groupID, err := id.New()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not create virtual group.")
		return
	}
	name := strings.TrimSpace(input.Name)
	now := database.Now()
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not create virtual group.")
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(r.Context(), `INSERT INTO namespaces(name,kind,entity_id) VALUES(?,'virtual',?)`, name, groupID)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO virtual_provider_groups(id,name,created_at,updated_at) VALUES(?,?,?,?)`, groupID, name, now, now)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at) SELECT id,'virtual',?,0,? FROM client_keys`, groupID, now)
	}
	if err != nil || tx.Commit() != nil {
		if database.IsConstraint(err) {
			adminError(w, 409, "name_conflict", "Provider and virtual group names share one namespace; choose another name.")
		} else {
			adminError(w, 500, "database_error", "Could not create virtual group.")
		}
		return
	}
	writeJSON(w, 201, map[string]any{"id": groupID, "name": name})
}

func (s *Server) updateVirtualGroup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name            string `json:"name"`
		ConfirmBreaking bool   `json:"confirm_breaking_change"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	groupID := r.PathValue("id")
	var oldName string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT name FROM namespaces WHERE entity_id=? AND kind='virtual'`, groupID).Scan(&oldName); err == sql.ErrNoRows {
		adminError(w, 404, "not_found", "Virtual group not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not rename virtual group.")
		return
	}
	if strings.TrimSpace(input.Name) != oldName && !input.ConfirmBreaking {
		adminError(w, 409, "breaking_change_confirmation_required", "Renaming changes every client-facing virtual model ID. Confirm the breaking change.")
		return
	}
	result, err := s.db.SQL.ExecContext(r.Context(), `UPDATE namespaces SET name=? WHERE entity_id=? AND kind='virtual'`, strings.TrimSpace(input.Name), groupID)
	if err != nil {
		if database.IsConstraint(err) {
			adminError(w, 409, "name_conflict", "That provider-group name is already in use.")
		} else {
			adminError(w, 500, "database_error", "Could not rename virtual group.")
		}
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		adminError(w, 404, "not_found", "Virtual group not found.")
		return
	}
	_, _ = s.db.SQL.ExecContext(r.Context(), `UPDATE virtual_provider_groups SET updated_at=? WHERE id=?`, database.Now(), r.PathValue("id"))
	w.WriteHeader(204)
}

func (s *Server) deleteVirtualGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not delete virtual group.")
		return
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM virtual_models WHERE virtual_group_id=?`, groupID).Scan(&count); err != nil {
		adminError(w, 500, "database_error", "Could not delete virtual group.")
		return
	}
	if count > 0 {
		adminError(w, 409, "group_not_empty", "Delete the group's virtual models first.")
		return
	}
	_, err = tx.ExecContext(r.Context(), `DELETE FROM client_group_defaults WHERE group_kind='virtual' AND group_id=?`, groupID)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `DELETE FROM virtual_provider_groups WHERE id=?`, groupID)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `DELETE FROM namespaces WHERE entity_id=? AND kind='virtual'`, groupID)
	}
	if err != nil || tx.Commit() != nil {
		adminError(w, 500, "database_error", "Could not delete virtual group.")
		return
	}
	w.WriteHeader(204)
}

type virtualModelView struct {
	ID                       string              `json:"id"`
	GroupID                  string              `json:"group_id"`
	GroupName                string              `json:"group_name"`
	Name                     string              `json:"name"`
	CanonicalModelID         string              `json:"canonical_model_id"`
	TargetProviderID         string              `json:"target_provider_id"`
	TargetProviderName       string              `json:"target_provider_name"`
	TargetModelID            string              `json:"target_model_id"`
	TargetUpstreamModelID    string              `json:"target_upstream_model_id"`
	RoutingMode              string              `json:"routing_mode"`
	Targets                  []virtualTargetView `json:"targets"`
	ContextLength            *int64              `json:"context_length"`
	MaxOutputTokens          *int64              `json:"max_output_tokens"`
	SupportsTools            *bool               `json:"supports_tools"`
	SupportsVision           *bool               `json:"supports_vision"`
	SupportsReasoning        *bool               `json:"supports_reasoning"`
	SupportsStructuredOutput *bool               `json:"supports_structured_output"`
	Available                bool                `json:"available"`
	Warning                  string              `json:"warning,omitempty"`
	CreatedAt                string              `json:"created_at"`
	UpdatedAt                string              `json:"updated_at"`
}

type virtualTargetView struct {
	ID                       string   `json:"id"`
	ProviderModelID          string   `json:"provider_model_id"`
	ProviderID               string   `json:"provider_id"`
	ProviderName             string   `json:"provider_name"`
	UpstreamModelID          string   `json:"upstream_model_id"`
	NativeProtocol           string   `json:"native_protocol,omitempty"`
	Position                 int      `json:"position"`
	Enabled                  bool     `json:"enabled"`
	Available                bool     `json:"available"`
	Warning                  string   `json:"warning,omitempty"`
	ContextLength            *int64   `json:"context_length"`
	MaxOutputTokens          *int64   `json:"max_output_tokens"`
	SupportsTools            *bool    `json:"supports_tools"`
	SupportsVision           *bool    `json:"supports_vision"`
	SupportsReasoning        *bool    `json:"supports_reasoning"`
	SupportsStructuredOutput *bool    `json:"supports_structured_output"`
	InputModalities          []string `json:"input_modalities,omitempty"`
	OutputModalities         []string `json:"output_modalities,omitempty"`
}

func (s *Server) listVirtualModels(w http.ResponseWriter, r *http.Request) {
	limit, offset, search := pagination(r)
	pattern := "%" + search + "%"
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT v.id,g.id,g.name,v.name,g.name||'/'||v.name,v.routing_mode,v.created_at,v.updated_at FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE g.name LIKE ? OR v.name LIKE ? OR EXISTS(SELECT 1 FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=v.id AND (p.name LIKE ? OR m.upstream_model_id LIKE ?)) ORDER BY g.name,v.name LIMIT ? OFFSET ?`, pattern, pattern, pattern, pattern, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not list virtual models.")
		return
	}
	defer rows.Close()
	data := []virtualModelView{}
	for rows.Next() {
		var v virtualModelView
		if rows.Scan(&v.ID, &v.GroupID, &v.GroupName, &v.Name, &v.CanonicalModelID, &v.RoutingMode, &v.CreatedAt, &v.UpdatedAt) != nil {
			adminError(w, 500, "database_error", "Could not list virtual models.")
			return
		}
		v.Targets, err = s.virtualTargets(r, v.ID)
		if err != nil {
			adminError(w, 500, "database_error", "Could not list virtual models.")
			return
		}
		eligible := eligibleVirtualTargets(v.Targets)
		var tools, vision, reasoning, structured []*bool
		for _, target := range eligible {
			v.Available = true
			tools = append(tools, target.SupportsTools)
			vision = append(vision, target.SupportsVision)
			reasoning = append(reasoning, target.SupportsReasoning)
			structured = append(structured, target.SupportsStructuredOutput)
		}
		// These legacy fields remain in the DTO for compatibility. They are
		// populated from the first ordered v2 target, but never drive any
		// dependency, availability, or capability calculation.
		if len(v.Targets) > 0 {
			primary := v.Targets[0]
			v.TargetProviderID, v.TargetProviderName, v.TargetModelID, v.TargetUpstreamModelID = primary.ProviderID, primary.ProviderName, primary.ProviderModelID, primary.UpstreamModelID
		}
		v.ContextLength = aggregateVirtualNumeric(v.Targets, func(target virtualTargetView) *int64 { return target.ContextLength })
		v.MaxOutputTokens = aggregateVirtualNumeric(v.Targets, func(target virtualTargetView) *int64 { return target.MaxOutputTokens })
		for _, target := range v.Targets {
			if target.Warning != "" {
				v.Warning = target.Warning
			}
		}
		v.SupportsTools = triStateANDBool(tools)
		v.SupportsVision = triStateANDBool(vision)
		v.SupportsReasoning = triStateANDBool(reasoning)
		v.SupportsStructuredOutput = triStateANDBool(structured)
		data = append(data, v)
	}
	if err := rows.Err(); err != nil {
		adminError(w, 500, "database_error", "Could not list virtual models.")
		return
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) virtualTargets(r *http.Request, virtualID string) ([]virtualTargetView, error) {
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT t.id,t.provider_model_id,p.id,p.name,m.upstream_model_id,coalesce(m.native_protocol,''),t.position,t.enabled,(p.enabled=1 AND m.available=1),CASE WHEN t.enabled=0 THEN 'Target is disabled' WHEN p.enabled=0 THEN 'Target provider is disabled' WHEN m.available=0 THEN 'Target model is retired' ELSE '' END,m.context_length,m.max_output_tokens,m.supports_tools,m.supports_vision,m.supports_reasoning,m.supports_structured_output,m.input_modalities,m.output_modalities FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=? ORDER BY t.position`, virtualID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []virtualTargetView{}
	for rows.Next() {
		var v virtualTargetView
		var enabled, available int
		var tools, vision, reasoning, structured sql.NullInt64
		var inputMod, outputMod sql.NullString
		if err := rows.Scan(&v.ID, &v.ProviderModelID, &v.ProviderID, &v.ProviderName, &v.UpstreamModelID, &v.NativeProtocol, &v.Position, &enabled, &available, &v.Warning, &v.ContextLength, &v.MaxOutputTokens, &tools, &vision, &reasoning, &structured, &inputMod, &outputMod); err != nil {
			return nil, err
		}
		v.Enabled, v.Available = scanBool(enabled), scanBool(available)
		v.SupportsTools = triBoolFromInt(tools)
		v.SupportsVision = triBoolFromInt(vision)
		v.SupportsReasoning = triBoolFromInt(reasoning)
		v.SupportsStructuredOutput = triBoolFromInt(structured)
		v.InputModalities = decodeModalities(inputMod)
		v.OutputModalities = decodeModalities(outputMod)
		data = append(data, v)
	}
	return data, rows.Err()
}

func (s *Server) createVirtualModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		GroupID          string               `json:"group_id"`
		GroupName        string               `json:"group_name"`
		Name             string               `json:"name"`
		TargetProviderID string               `json:"target_provider_id"`
		TargetModelID    string               `json:"target_model_id"`
		RoutingMode      string               `json:"routing_mode"`
		Targets          []virtualTargetInput `json:"targets"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	if len(input.Targets) == 0 && input.TargetModelID != "" {
		input.Targets = []virtualTargetInput{{ProviderModelID: input.TargetModelID, Enabled: true}}
	}
	if input.RoutingMode == "" {
		input.RoutingMode = "fixed"
	}
	if err := validateVirtualTargets(input.RoutingMode, input.Targets); err != nil {
		adminError(w, 400, "invalid_targets", err.Error())
		return
	}
	virtualID, err := id.New()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not create virtual model.")
		return
	}
	now := database.Now()
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not create virtual model.")
		return
	}
	defer tx.Rollback()
	groupID := input.GroupID
	if groupID == "" {
		groupName := strings.TrimSpace(input.GroupName)
		if groupName == "" {
			adminError(w, 400, "invalid_request", "A virtual group is required; provide group_id or group_name.")
			return
		}
		groupID, err = id.New()
		if err != nil {
			adminError(w, 500, "internal_error", "Could not create virtual model.")
			return
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO namespaces(name,kind,entity_id) VALUES(?,'virtual',?)`, groupName, groupID)
		if err == nil {
			_, err = tx.ExecContext(r.Context(), `INSERT INTO virtual_provider_groups(id,name,created_at,updated_at) VALUES(?,?,?,?)`, groupID, groupName, now, now)
		}
		if err == nil {
			_, err = tx.ExecContext(r.Context(), `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at) SELECT id,'virtual',?,0,? FROM client_keys`, groupID, now)
		}
		if err != nil {
			if database.IsConstraint(err) {
				adminError(w, 409, "name_conflict", "Provider and virtual group names share one namespace; choose another name.")
			} else {
				adminError(w, 500, "database_error", "Could not create virtual model.")
			}
			return
		}
	}
	primary := input.Targets[0].ProviderModelID
	var primaryProvider string
	if err = tx.QueryRowContext(r.Context(), `SELECT provider_id FROM provider_models WHERE id=?`, primary).Scan(&primaryProvider); err != nil {
		adminError(w, 400, "invalid_target", "Target model does not exist.")
		return
	}
	_, err = tx.ExecContext(r.Context(), `INSERT INTO virtual_models(id,virtual_group_id,name,target_provider_id,target_provider_model_id,routing_mode,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, virtualID, groupID, input.Name, primaryProvider, primary, input.RoutingMode, now, now)
	if err == nil {
		err = replaceVirtualTargets(r, tx, virtualID, input.Targets, now)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at) SELECT c.id,'virtual',?,coalesce(d.new_models_enabled,0),?,? FROM client_keys c LEFT JOIN client_group_defaults d ON d.client_key_id=c.id AND d.group_kind='virtual' AND d.group_id=?`, virtualID, now, now, groupID)
	}
	if err != nil || tx.Commit() != nil {
		if database.IsConstraint(err) {
			adminError(w, 409, "model_conflict", "That virtual model name already exists in the group.")
		} else {
			adminError(w, 500, "database_error", "Could not create virtual model.")
		}
		return
	}
	writeJSON(w, 201, map[string]any{"id": virtualID})
}

func (s *Server) updateVirtualModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name             *string              `json:"name"`
		TargetProviderID *string              `json:"target_provider_id"`
		TargetModelID    *string              `json:"target_model_id"`
		ConfirmBreaking  bool                 `json:"confirm_breaking_change"`
		RoutingMode      *string              `json:"routing_mode"`
		Targets          []virtualTargetInput `json:"targets"`
		FixedTargetID    string               `json:"fixed_target_id"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	modelID := r.PathValue("id")
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not update virtual model.")
		return
	}
	defer tx.Rollback()
	var oldName, currentProvider, currentModel, currentMode string
	if err = tx.QueryRowContext(r.Context(), `SELECT name,target_provider_id,target_provider_model_id,routing_mode FROM virtual_models WHERE id=?`, modelID).Scan(&oldName, &currentProvider, &currentModel, &currentMode); err == sql.ErrNoRows {
		adminError(w, 404, "not_found", "Virtual model not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not update virtual model.")
		return
	}
	if input.Name != nil && *input.Name != oldName && !input.ConfirmBreaking {
		adminError(w, 409, "breaking_change_confirmation_required", "Renaming changes the client-facing model ID. Confirm the breaking change.")
		return
	}
	if input.TargetProviderID != nil {
		currentProvider = *input.TargetProviderID
	}
	if input.TargetModelID != nil {
		currentModel = *input.TargetModelID
	}
	if len(input.Targets) == 0 && (input.TargetProviderID != nil || input.TargetModelID != nil) {
		input.Targets = []virtualTargetInput{{ProviderModelID: currentModel, Enabled: true}}
	}
	newMode := currentMode
	if input.RoutingMode != nil {
		newMode = *input.RoutingMode
	}
	if newMode == "fixed" && currentMode == "ordered_fallback" && (input.FixedTargetID == "" || len(input.Targets) == 0) {
		adminError(w, 400, "fixed_target_required", "Choose the target that remains active in Fixed mode.")
		return
	}
	if len(input.Targets) > 0 {
		if input.FixedTargetID != "" {
			for i := range input.Targets {
				input.Targets[i].Enabled = input.Targets[i].ProviderModelID == input.FixedTargetID
			}
		}
		if err := validateVirtualTargets(newMode, input.Targets); err != nil {
			adminError(w, 400, "invalid_targets", err.Error())
			return
		}
		currentModel = input.Targets[0].ProviderModelID
		if err = tx.QueryRowContext(r.Context(), `SELECT provider_id FROM provider_models WHERE id=?`, currentModel).Scan(&currentProvider); err != nil {
			adminError(w, 400, "invalid_target", "Target model does not exist.")
			return
		}
	}
	newName := oldName
	if input.Name != nil {
		newName = *input.Name
	}
	now := database.Now()
	_, err = tx.ExecContext(r.Context(), `UPDATE virtual_models SET name=?,target_provider_id=?,target_provider_model_id=?,routing_mode=?,updated_at=? WHERE id=?`, newName, currentProvider, currentModel, newMode, now, modelID)
	if err == nil && len(input.Targets) > 0 {
		err = replaceVirtualTargets(r, tx, modelID, input.Targets, now)
	}
	if err != nil || tx.Commit() != nil {
		if database.IsConstraint(err) {
			adminError(w, 409, "model_conflict", "That virtual model name already exists in the group.")
		} else {
			adminError(w, 500, "database_error", "Could not update virtual model.")
		}
		return
	}
	w.WriteHeader(204)
}

func (s *Server) deleteVirtualModel(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("id")
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not delete virtual model.")
		return
	}
	defer tx.Rollback()
	var bindings int
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM client_single_bindings b JOIN client_keys c ON c.id = b.client_key_id WHERE b.virtual_model_id=? AND c.key_type='single'`, modelID).Scan(&bindings); err != nil {
		adminError(w, 500, "database_error", "Could not delete virtual model.")
		return
	}
	if bindings > 0 {
		adminError(w, 409, "single_binding_in_use", "Repoint Single client keys using this virtual model first.")
		return
	}
	// Catalogue-type client keys may carry a stale client_single_bindings row
	// from a prior Single-key configuration. Such rows are inert (catalogue
	// keys are resolved through client_model_permissions, not the binding),
	// but their ON DELETE RESTRICT foreign key would otherwise block this
	// delete. Drop them explicitly so the spec's "Single keys" intent wins.
	if _, err = tx.ExecContext(r.Context(), `DELETE FROM client_single_bindings WHERE virtual_model_id=? AND client_key_id IN (SELECT id FROM client_keys WHERE key_type='catalogue')`, modelID); err != nil {
		adminError(w, 500, "database_error", "Could not delete virtual model.")
		return
	}
	_, err = tx.ExecContext(r.Context(), `DELETE FROM client_model_permissions WHERE model_kind='virtual' AND model_id=?`, modelID)
	if err == nil {
		result, e := tx.ExecContext(r.Context(), `DELETE FROM virtual_models WHERE id=?`, modelID)
		err = e
		if err == nil {
			n, _ := result.RowsAffected()
			if n == 0 {
				adminError(w, 404, "not_found", "Virtual model not found.")
				return
			}
		}
	}
	if err != nil || tx.Commit() != nil {
		adminError(w, 500, "database_error", "Could not delete virtual model.")
		return
	}
	w.WriteHeader(204)
}
