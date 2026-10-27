package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/store"
)

// Catalogue templates are saved permission matrices that Catalogue client
// keys link to. Template names are admin-only labels and never reach clients.

const maxTemplateNameLength = 100

type templateView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	ClientCount int    `json:"client_count"`
}

func newTemplateView(row store.TemplateRow) templateView {
	return templateView{ID: row.ID, Name: row.Name, Description: row.Description, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ClientCount: row.ClientCount}
}

func validTemplateName(name string) bool {
	return name != "" && len(name) <= maxTemplateNameLength
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := s.scope(r).ListTemplates(r.Context())
	if err != nil {
		adminError(w, 500, "database_error", "Could not list catalogue templates.")
		return
	}
	data := make([]templateView, 0, len(rows))
	for _, row := range rows {
		data = append(data, newTemplateView(row))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request) {
	row, err := s.scope(r).GetTemplate(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrTemplateNotFound) {
		adminError(w, 404, "not_found", "Catalogue template not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not load catalogue template.")
		return
	}
	writeJSON(w, 200, newTemplateView(row))
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name           string `json:"name"`
		Description    string `json:"description"`
		StartFrom      string `json:"start_from"`
		SourceClientID string `json:"source_client_id"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if !validTemplateName(input.Name) {
		adminError(w, 400, "invalid_name", "Template names must be 1-100 characters.")
		return
	}
	if input.StartFrom == "" {
		input.StartFrom = store.TemplateStartEmpty
	}
	switch input.StartFrom {
	case store.TemplateStartEmpty, store.TemplateStartAll:
	case store.TemplateStartClient:
		if input.SourceClientID == "" {
			adminError(w, 400, "invalid_source_client", "Choose a client to copy.")
			return
		}
	default:
		adminError(w, 400, "invalid_start_from", "Start from must be empty, all or client.")
		return
	}
	templateID, err := id.New()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not generate catalogue template.")
		return
	}
	err = s.scope(r).CreateTemplate(r.Context(), store.CreateTemplateInput{ID: templateID, Name: input.Name, Description: input.Description, StartFrom: input.StartFrom, SourceClientID: input.SourceClientID})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrClientKeyNotFound):
			adminError(w, 400, "invalid_source_client", "The client to copy does not exist.")
		case database.IsConstraint(err):
			adminError(w, 409, "name_conflict", "A catalogue template with that name already exists.")
		default:
			adminError(w, 500, "database_error", "Could not create catalogue template.")
		}
		return
	}
	s.recordResourceAudit(r, s.scope(r).AccountID(), store.AuditEvent{
		Event:      "catalogue_template.created",
		TargetType: "catalogue_template",
		TargetID:   templateID,
		Metadata:   map[string]string{"name": input.Name, "start_from": input.StartFrom},
	})
	writeJSON(w, 201, map[string]any{"id": templateID, "name": input.Name})
}

func (s *Server) updateTemplate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if !validTemplateName(name) {
			adminError(w, 400, "invalid_name", "Template names must be 1-100 characters.")
			return
		}
		input.Name = &name
	}
	templateID := r.PathValue("id")
	err := s.scope(r).UpdateTemplate(r.Context(), templateID, input.Name, input.Description)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrTemplateNotFound):
			adminError(w, 404, "not_found", "Catalogue template not found.")
		case database.IsConstraint(err):
			adminError(w, 409, "name_conflict", "A catalogue template with that name already exists.")
		default:
			adminError(w, 500, "database_error", "Could not update catalogue template.")
		}
		return
	}
	s.recordResourceAudit(r, s.scope(r).AccountID(), store.AuditEvent{
		Event:      "catalogue_template.updated",
		TargetType: "catalogue_template",
		TargetID:   templateID,
	})
	w.WriteHeader(204)
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("id")
	detached, err := s.scope(r).DeleteTemplate(r.Context(), templateID)
	if errors.Is(err, store.ErrTemplateNotFound) {
		adminError(w, 404, "not_found", "Catalogue template not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not delete catalogue template.")
		return
	}
	for _, clientID := range detached {
		s.clients.Invalidate(clientID)
	}
	s.recordResourceAudit(r, s.scope(r).AccountID(), store.AuditEvent{
		Event:      "catalogue_template.deleted",
		TargetType: "catalogue_template",
		TargetID:   templateID,
		Metadata:   map[string]string{"detached_clients": strconv.Itoa(len(detached))},
	})
	w.WriteHeader(204)
}

func (s *Server) getTemplatePermissions(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("id")
	groups, err := s.scope(r).ListTemplatePermissions(r.Context(), templateID)
	if errors.Is(err, store.ErrTemplateNotFound) {
		adminError(w, 404, "not_found", "Catalogue template not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not load permissions.")
		return
	}
	writeJSON(w, 200, map[string]any{"template_id": templateID, "groups": permissionGroupsView(groups), "feeder_explanation": feederExplanation})
}

func (s *Server) updateTemplatePermissions(w http.ResponseWriter, r *http.Request) {
	defaults, permissions, ok := decodePermissionUpdates(w, r)
	if !ok {
		return
	}
	templateID := r.PathValue("id")
	linked, err := s.scope(r).UpdateTemplatePermissions(r.Context(), templateID, defaults, permissions)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrTemplateNotFound):
			adminError(w, 404, "not_found", "Catalogue template not found.")
		case errors.Is(err, store.ErrPermissionGroupNotFound):
			adminError(w, 400, "invalid_permission", "Unknown permission group.")
		case errors.Is(err, store.ErrPermissionModelNotFound):
			adminError(w, 400, "invalid_permission", "Unknown model permission.")
		default:
			adminError(w, 500, "database_error", "Could not update permissions.")
		}
		return
	}
	for _, clientID := range linked {
		s.clients.Invalidate(clientID)
	}
	s.recordResourceAudit(r, s.scope(r).AccountID(), store.AuditEvent{
		Event:      "catalogue_template.permissions_updated",
		TargetType: "catalogue_template",
		TargetID:   templateID,
		Metadata:   map[string]string{"defaults": strconv.Itoa(len(defaults)), "permissions": strconv.Itoa(len(permissions)), "linked_clients": strconv.Itoa(len(linked))},
	})
	w.WriteHeader(204)
}
