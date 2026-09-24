package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/store"
)

type virtualTargetInput struct {
	ProviderModelID string `json:"provider_model_id"`
	Enabled         bool   `json:"enabled"`
}

const maxVirtualTargets = 16

func validateVirtualTargets(mode string, targets []virtualTargetInput) error {
	if mode != "fixed" && mode != "ordered_fallback" {
		return fmt.Errorf("routing_mode must be fixed or ordered_fallback")
	}
	if len(targets) == 0 {
		return fmt.Errorf("at least one target is required")
	}
	if mode == "ordered_fallback" && len(targets) > maxVirtualTargets {
		return fmt.Errorf("ordered fallback supports at most %d targets", maxVirtualTargets)
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

func toStoreTargets(targets []virtualTargetInput) []store.VirtualTargetInput {
	out := make([]store.VirtualTargetInput, 0, len(targets))
	for _, t := range targets {
		out = append(out, store.VirtualTargetInput{ProviderModelID: t.ProviderModelID, Enabled: t.Enabled})
	}
	return out
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
	rows, err := s.scope(r).ListVirtualGroups(r.Context(), store.ListFilter{Search: search, Limit: limit, Offset: offset})
	if err != nil {
		adminError(w, 500, "database_error", "Could not list virtual groups.")
		return
	}
	data := make([]virtualGroupView, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		data = append(data, virtualGroupView{ID: row.ID, Name: row.Name, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ModelCount: row.ModelCount})
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
	if err := s.scope(r).CreateVirtualGroup(r.Context(), groupID, name); err != nil {
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
	sc := s.scope(r)
	oldName, err := sc.VirtualGroupName(r.Context(), groupID)
	if errors.Is(err, store.ErrVirtualGroupNotFound) {
		adminError(w, 404, "not_found", "Virtual group not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not rename virtual group.")
		return
	}
	name := strings.TrimSpace(input.Name)
	if name != oldName && !input.ConfirmBreaking {
		adminError(w, 409, "breaking_change_confirmation_required", "Renaming changes every client-facing virtual model ID. Confirm the breaking change.")
		return
	}
	err = sc.UpdateVirtualGroup(r.Context(), groupID, name)
	if errors.Is(err, store.ErrVirtualGroupNotFound) {
		adminError(w, 404, "not_found", "Virtual group not found.")
		return
	}
	if err != nil {
		if database.IsConstraint(err) {
			adminError(w, 409, "name_conflict", "That provider-group name is already in use.")
		} else {
			adminError(w, 500, "database_error", "Could not rename virtual group.")
		}
		return
	}
	w.WriteHeader(204)
}

func (s *Server) deleteVirtualGroup(w http.ResponseWriter, r *http.Request) {
	err := s.scope(r).DeleteVirtualGroup(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrVirtualGroupNotEmpty):
		adminError(w, 409, "group_not_empty", "Delete the group's virtual models first.")
		return
	case errors.Is(err, store.ErrVirtualGroupNotFound):
		adminError(w, 404, "not_found", "Virtual group not found.")
		return
	case err != nil:
		adminError(w, 500, "database_error", "Could not delete virtual group.")
		return
	}
	w.WriteHeader(204)
}

type virtualModelView struct {
	ID                       string                           `json:"id"`
	GroupID                  string                           `json:"group_id"`
	GroupName                string                           `json:"group_name"`
	Name                     string                           `json:"name"`
	CanonicalModelID         string                           `json:"canonical_model_id"`
	TargetProviderID         string                           `json:"target_provider_id"`
	TargetProviderName       string                           `json:"target_provider_name"`
	TargetModelID            string                           `json:"target_model_id"`
	TargetUpstreamModelID    string                           `json:"target_upstream_model_id"`
	RoutingMode              string                           `json:"routing_mode"`
	Targets                  []virtualTargetView              `json:"targets"`
	ContextLength            *int64                           `json:"context_length"`
	MaxOutputTokens          *int64                           `json:"max_output_tokens"`
	SupportsTools            *bool                            `json:"supports_tools"`
	SupportsVision           *bool                            `json:"supports_vision"`
	SupportsReasoning        *bool                            `json:"supports_reasoning"`
	SupportsStructuredOutput *bool                            `json:"supports_structured_output"`
	ReasoningCapabilities    *providers.ReasoningCapabilities `json:"reasoning_capabilities,omitempty"`
	Available                bool                             `json:"available"`
	Warning                  string                           `json:"warning,omitempty"`
	CreatedAt                string                           `json:"created_at"`
	UpdatedAt                string                           `json:"updated_at"`
}

type virtualTargetView struct {
	ID                       string                           `json:"id"`
	ProviderModelID          string                           `json:"provider_model_id"`
	ProviderID               string                           `json:"provider_id"`
	ProviderName             string                           `json:"provider_name"`
	UpstreamModelID          string                           `json:"upstream_model_id"`
	NativeProtocol           string                           `json:"native_protocol,omitempty"`
	Position                 int                              `json:"position"`
	Enabled                  bool                             `json:"enabled"`
	Available                bool                             `json:"available"`
	Warning                  string                           `json:"warning,omitempty"`
	ContextLength            *int64                           `json:"context_length"`
	MaxOutputTokens          *int64                           `json:"max_output_tokens"`
	SupportsTools            *bool                            `json:"supports_tools"`
	SupportsVision           *bool                            `json:"supports_vision"`
	SupportsReasoning        *bool                            `json:"supports_reasoning"`
	SupportsStructuredOutput *bool                            `json:"supports_structured_output"`
	ReasoningCapabilities    *providers.ReasoningCapabilities `json:"reasoning_capabilities,omitempty"`
	InputModalities          []string                         `json:"input_modalities,omitempty"`
	OutputModalities         []string                         `json:"output_modalities,omitempty"`
}

func (s *Server) listVirtualModels(w http.ResponseWriter, r *http.Request) {
	limit, offset, search := pagination(r)
	sc := s.scope(r)
	rows, err := sc.ListVirtualModels(r.Context(), store.ListFilter{Search: search, Limit: limit, Offset: offset})
	if err != nil {
		adminError(w, 500, "database_error", "Could not list virtual models.")
		return
	}
	data := make([]virtualModelView, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		v := virtualModelView{ID: row.ID, GroupID: row.GroupID, GroupName: row.GroupName, Name: row.Name, CanonicalModelID: row.CanonicalModelID, RoutingMode: row.RoutingMode, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
		v.Targets, err = s.virtualTargets(r, v.ID)
		if err != nil {
			adminError(w, 500, "database_error", "Could not list virtual models.")
			return
		}
		eligible := eligibleVirtualTargets(v.Targets)
		capabilities := make([]virtualTargetCapabilities, 0, len(eligible))
		for _, target := range eligible {
			v.Available = true
			capabilities = append(capabilities, virtualTargetCapability(target))
		}
		// These legacy fields remain in the DTO for compatibility. They are
		// populated from the first ordered v2 target, but never drive any
		// dependency, availability, or capability calculation.
		if len(v.Targets) > 0 {
			primary := v.Targets[0]
			v.TargetProviderID, v.TargetProviderName, v.TargetModelID, v.TargetUpstreamModelID = primary.ProviderID, primary.ProviderName, primary.ProviderModelID, primary.UpstreamModelID
		}
		aggregated := aggregateVirtualCapabilities(capabilities)
		v.ContextLength = aggregated.ContextLength
		v.MaxOutputTokens = aggregated.MaxOutputTokens
		for _, target := range v.Targets {
			if target.Warning != "" {
				v.Warning = target.Warning
			}
		}
		v.SupportsTools = aggregated.SupportsTools
		v.SupportsVision = aggregated.SupportsVision
		v.SupportsReasoning = aggregated.SupportsReasoning
		v.SupportsStructuredOutput = aggregated.SupportsStructuredOutput
		v.ReasoningCapabilities = aggregated.ReasoningCapabilities
		data = append(data, v)
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) virtualTargets(r *http.Request, virtualID string) ([]virtualTargetView, error) {
	rows, err := s.scope(r).VirtualTargets(r.Context(), virtualID)
	if err != nil {
		return nil, err
	}
	data := make([]virtualTargetView, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		v := virtualTargetView{
			ID:              row.ID,
			ProviderModelID: row.ProviderModelID,
			ProviderID:      row.ProviderID,
			ProviderName:    row.ProviderName,
			UpstreamModelID: row.UpstreamModelID,
			NativeProtocol:  row.NativeProtocol,
			Position:        row.Position,
			Enabled:         row.Enabled,
			Available:       row.Available,
			Warning:         row.Warning,
			ContextLength:   row.ContextLength,
			MaxOutputTokens: row.MaxOutputTokens,
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
	return data, nil
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
	groupID := input.GroupID
	newGroupID := ""
	groupName := ""
	if groupID == "" {
		groupName = strings.TrimSpace(input.GroupName)
		if groupName == "" {
			adminError(w, 400, "invalid_request", "A virtual group is required; provide group_id or group_name.")
			return
		}
		newGroupID, err = id.New()
		if err != nil {
			adminError(w, 500, "internal_error", "Could not create virtual model.")
			return
		}
	}
	err = s.scope(r).CreateVirtualModel(r.Context(), store.CreateVirtualModelInput{
		ID:          virtualID,
		GroupID:     groupID,
		NewGroupID:  newGroupID,
		GroupName:   groupName,
		Name:        input.Name,
		RoutingMode: input.RoutingMode,
		Targets:     toStoreTargets(input.Targets),
	})
	switch {
	case errors.Is(err, store.ErrVirtualTargetNotFound):
		adminError(w, 400, "invalid_target", "Target model does not exist.")
		return
	case err != nil && database.IsConstraint(err):
		if newGroupID != "" {
			adminError(w, 409, "name_conflict", "Provider and virtual group names share one namespace; choose another name.")
		} else {
			adminError(w, 409, "model_conflict", "That virtual model name already exists in the group.")
		}
		return
	case err != nil && writeLimitExceeded(w, err):
		return
	case err != nil:
		adminError(w, 500, "database_error", "Could not create virtual model.")
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
	sc := s.scope(r)
	current, err := sc.GetVirtualModelEditable(r.Context(), modelID)
	if errors.Is(err, store.ErrVirtualModelNotFound) {
		adminError(w, 404, "not_found", "Virtual model not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not update virtual model.")
		return
	}
	oldName := current.Name
	currentProvider, currentModel, currentMode := current.TargetProviderID, current.TargetModelID, current.RoutingMode
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
	}
	newName := oldName
	if input.Name != nil {
		newName = *input.Name
	}
	err = sc.UpdateVirtualModel(r.Context(), store.UpdateVirtualModelInput{
		ID:             modelID,
		Name:           newName,
		TargetProvider: currentProvider,
		TargetModel:    currentModel,
		RoutingMode:    newMode,
		ReplaceTargets: len(input.Targets) > 0,
		Targets:        toStoreTargets(input.Targets),
	})
	switch {
	case errors.Is(err, store.ErrVirtualTargetNotFound):
		adminError(w, 400, "invalid_target", "Target model does not exist.")
		return
	case errors.Is(err, store.ErrVirtualModelNotFound):
		adminError(w, 404, "not_found", "Virtual model not found.")
		return
	case err != nil && database.IsConstraint(err):
		adminError(w, 409, "model_conflict", "That virtual model name already exists in the group.")
		return
	case err != nil:
		adminError(w, 500, "database_error", "Could not update virtual model.")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) deleteVirtualModel(w http.ResponseWriter, r *http.Request) {
	err := s.scope(r).DeleteVirtualModel(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrVirtualBindingInUse):
		adminError(w, 409, "single_binding_in_use", "Repoint Single client keys using this virtual model first.")
		return
	case errors.Is(err, store.ErrVirtualModelNotFound):
		adminError(w, 404, "not_found", "Virtual model not found.")
		return
	case err != nil:
		adminError(w, 500, "database_error", "Could not delete virtual model.")
		return
	}
	w.WriteHeader(204)
}
