package server

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
)

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
	if !input.ConfirmBreaking {
		adminError(w, 409, "breaking_change_confirmation_required", "Renaming changes every client-facing virtual model ID. Confirm the breaking change.")
		return
	}
	result, err := s.db.SQL.ExecContext(r.Context(), `UPDATE namespaces SET name=? WHERE entity_id=? AND kind='virtual'`, strings.TrimSpace(input.Name), r.PathValue("id"))
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
	ID                    string `json:"id"`
	GroupID               string `json:"group_id"`
	GroupName             string `json:"group_name"`
	Name                  string `json:"name"`
	CanonicalModelID      string `json:"canonical_model_id"`
	TargetProviderID      string `json:"target_provider_id"`
	TargetProviderName    string `json:"target_provider_name"`
	TargetModelID         string `json:"target_model_id"`
	TargetUpstreamModelID string `json:"target_upstream_model_id"`
	Available             bool   `json:"available"`
	Warning               string `json:"warning,omitempty"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
}

func (s *Server) listVirtualModels(w http.ResponseWriter, r *http.Request) {
	limit, offset, search := pagination(r)
	pattern := "%" + search + "%"
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT v.id,g.id,g.name,v.name,g.name||'/'||v.name,p.id,p.name,m.id,m.upstream_model_id,(p.enabled=1 AND m.available=1),CASE WHEN p.enabled=0 THEN 'Target provider is disabled' WHEN m.available=0 THEN 'Target model is retired' ELSE '' END,v.created_at,v.updated_at FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id JOIN providers p ON p.id=v.target_provider_id JOIN provider_models m ON m.id=v.target_provider_model_id WHERE g.name LIKE ? OR v.name LIKE ? OR p.name LIKE ? OR m.upstream_model_id LIKE ? ORDER BY g.name,v.name LIMIT ? OFFSET ?`, pattern, pattern, pattern, pattern, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not list virtual models.")
		return
	}
	defer rows.Close()
	data := []virtualModelView{}
	for rows.Next() {
		var v virtualModelView
		var available int
		if rows.Scan(&v.ID, &v.GroupID, &v.GroupName, &v.Name, &v.CanonicalModelID, &v.TargetProviderID, &v.TargetProviderName, &v.TargetModelID, &v.TargetUpstreamModelID, &available, &v.Warning, &v.CreatedAt, &v.UpdatedAt) != nil {
			adminError(w, 500, "database_error", "Could not list virtual models.")
			return
		}
		v.Available = scanBool(available)
		data = append(data, v)
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) createVirtualModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		GroupID          string `json:"group_id"`
		Name             string `json:"name"`
		TargetProviderID string `json:"target_provider_id"`
		TargetModelID    string `json:"target_model_id"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
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
	var valid int
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM provider_models WHERE id=? AND provider_id=?`, input.TargetModelID, input.TargetProviderID).Scan(&valid); err != nil || valid != 1 {
		adminError(w, 400, "invalid_target", "Target model does not belong to the selected provider.")
		return
	}
	_, err = tx.ExecContext(r.Context(), `INSERT INTO virtual_models(id,virtual_group_id,name,target_provider_id,target_provider_model_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, virtualID, input.GroupID, input.Name, input.TargetProviderID, input.TargetModelID, now, now)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at) SELECT c.id,'virtual',?,coalesce(d.new_models_enabled,0),?,? FROM client_keys c LEFT JOIN client_group_defaults d ON d.client_key_id=c.id AND d.group_kind='virtual' AND d.group_id=?`, virtualID, now, now, input.GroupID)
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
		Name             *string `json:"name"`
		TargetProviderID *string `json:"target_provider_id"`
		TargetModelID    *string `json:"target_model_id"`
		ConfirmBreaking  bool    `json:"confirm_breaking_change"`
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
	var oldName, currentProvider, currentModel string
	if err = tx.QueryRowContext(r.Context(), `SELECT name,target_provider_id,target_provider_model_id FROM virtual_models WHERE id=?`, modelID).Scan(&oldName, &currentProvider, &currentModel); err == sql.ErrNoRows {
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
	var valid int
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM provider_models WHERE id=? AND provider_id=?`, currentModel, currentProvider).Scan(&valid); err != nil || valid != 1 {
		adminError(w, 400, "invalid_target", "Target model does not belong to the selected provider.")
		return
	}
	newName := oldName
	if input.Name != nil {
		newName = *input.Name
	}
	_, err = tx.ExecContext(r.Context(), `UPDATE virtual_models SET name=?,target_provider_id=?,target_provider_model_id=?,updated_at=? WHERE id=?`, newName, currentProvider, currentModel, database.Now(), modelID)
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
