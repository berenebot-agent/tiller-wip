package server

import (
	"database/sql"
	"net/http"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
)

type clientKeyView struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Fingerprint string  `json:"fingerprint"`
	Enabled     bool    `json:"enabled"`
	CreatedAt   string  `json:"created_at"`
	RotatedAt   *string `json:"rotated_at"`
	UpdatedAt   string  `json:"updated_at"`
}

func (s *Server) listClientKeys(w http.ResponseWriter, r *http.Request) {
	limit, offset, search := pagination(r)
	pattern := "%" + search + "%"
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT id,name,description,secret_fingerprint,enabled,created_at,rotated_at,updated_at FROM client_keys WHERE name LIKE ? OR description LIKE ? ORDER BY name LIMIT ? OFFSET ?`, pattern, pattern, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not list client keys.")
		return
	}
	defer rows.Close()
	data := []clientKeyView{}
	for rows.Next() {
		var v clientKeyView
		var enabled int
		if rows.Scan(&v.ID, &v.Name, &v.Description, &v.Fingerprint, &enabled, &v.CreatedAt, &v.RotatedAt, &v.UpdatedAt) != nil {
			adminError(w, 500, "database_error", "Could not list client keys.")
			return
		}
		v.Enabled = scanBool(enabled)
		data = append(data, v)
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) createClientKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	generated, err := auth.GenerateKey()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not generate client key.")
		return
	}
	clientID, err := id.New()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not generate client key.")
		return
	}
	now := database.Now()
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not create client key.")
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(r.Context(), `INSERT INTO client_keys(id,name,description,selector,secret_hash,secret_fingerprint,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,1,?,?)`, clientID, input.Name, input.Description, generated.Selector, generated.Hash, generated.Fingerprint, now, now)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at) SELECT ?,'real',id,0,? FROM providers`, clientID, now)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at) SELECT ?,'virtual',id,0,? FROM virtual_provider_groups`, clientID, now)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at) SELECT ?,'real',id,0,?,? FROM provider_models`, clientID, now, now)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at) SELECT ?,'virtual',id,0,?,? FROM virtual_models`, clientID, now, now)
	}
	if err != nil || tx.Commit() != nil {
		if database.IsConstraint(err) {
			adminError(w, 409, "name_conflict", "A client key with that name already exists.")
		} else {
			adminError(w, 500, "database_error", "Could not create client key.")
		}
		return
	}
	writeJSON(w, 201, map[string]any{"id": clientID, "name": input.Name, "secret": generated.Plaintext, "fingerprint": generated.Fingerprint, "warning": "Copy this key now. It cannot be displayed again."})
}

func (s *Server) updateClientKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Enabled     *bool   `json:"enabled"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	clientID := r.PathValue("id")
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not update client key.")
		return
	}
	defer tx.Rollback()
	var name, description string
	var enabled int
	if err = tx.QueryRowContext(r.Context(), `SELECT name,description,enabled FROM client_keys WHERE id=?`, clientID).Scan(&name, &description, &enabled); err == sql.ErrNoRows {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not update client key.")
		return
	}
	if input.Name != nil {
		name = *input.Name
	}
	if input.Description != nil {
		description = *input.Description
	}
	if input.Enabled != nil {
		enabled = boolInt(*input.Enabled)
	}
	_, err = tx.ExecContext(r.Context(), `UPDATE client_keys SET name=?,description=?,enabled=?,updated_at=? WHERE id=?`, name, description, enabled, database.Now(), clientID)
	if err != nil || tx.Commit() != nil {
		if database.IsConstraint(err) {
			adminError(w, 409, "name_conflict", "A client key with that name already exists.")
		} else {
			adminError(w, 500, "database_error", "Could not update client key.")
		}
		return
	}
	s.clients.Invalidate(clientID)
	w.WriteHeader(204)
}

func (s *Server) rotateClientKey(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	generated, err := auth.GenerateKey()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not rotate client key.")
		return
	}
	now := database.Now()
	result, err := s.db.SQL.ExecContext(r.Context(), `UPDATE client_keys SET selector=?,secret_hash=?,secret_fingerprint=?,rotated_at=?,updated_at=? WHERE id=?`, generated.Selector, generated.Hash, generated.Fingerprint, now, now, clientID)
	if err != nil {
		adminError(w, 500, "database_error", "Could not rotate client key.")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	s.clients.Invalidate(clientID)
	writeJSON(w, 200, map[string]any{"id": clientID, "secret": generated.Plaintext, "fingerprint": generated.Fingerprint, "warning": "Copy this key now. The previous key is already invalid and this one cannot be displayed again."})
}

func (s *Server) deleteClientKey(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	result, err := s.db.SQL.ExecContext(r.Context(), `DELETE FROM client_keys WHERE id=?`, clientID)
	if err != nil {
		adminError(w, 500, "database_error", "Could not delete client key.")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	s.clients.Invalidate(clientID)
	w.WriteHeader(204)
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
	var exists int
	if s.db.SQL.QueryRowContext(r.Context(), `SELECT count(*) FROM client_keys WHERE id=?`, clientID).Scan(&exists) != nil || exists == 0 {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	groups := []permissionGroup{}
	realRows, err := s.db.SQL.QueryContext(r.Context(), `SELECT p.id,p.name,coalesce(d.new_models_enabled,0) FROM providers p LEFT JOIN client_group_defaults d ON d.client_key_id=? AND d.group_kind='real' AND d.group_id=p.id ORDER BY p.name`, clientID)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load permissions.")
		return
	}
	for realRows.Next() {
		var g permissionGroup
		var feeder int
		g.Kind = "real"
		if realRows.Scan(&g.ID, &g.Name, &feeder) != nil {
			realRows.Close()
			adminError(w, 500, "database_error", "Could not load permissions.")
			return
		}
		g.NewModelsEnabled = scanBool(feeder)
		g.Models = []permissionModel{}
		rows, e := s.db.SQL.QueryContext(r.Context(), `SELECT m.id,p.name||'/'||m.upstream_model_id,coalesce(x.enabled,0),m.available FROM provider_models m JOIN providers p ON p.id=m.provider_id LEFT JOIN client_model_permissions x ON x.client_key_id=? AND x.model_kind='real' AND x.model_id=m.id WHERE m.provider_id=? ORDER BY m.upstream_model_id`, clientID, g.ID)
		if e != nil {
			realRows.Close()
			adminError(w, 500, "database_error", "Could not load permissions.")
			return
		}
		for rows.Next() {
			var m permissionModel
			var enabled, available int
			m.Kind = "real"
			_ = rows.Scan(&m.ID, &m.CanonicalModelID, &enabled, &available)
			m.Enabled = scanBool(enabled)
			m.Available = scanBool(available)
			g.Models = append(g.Models, m)
		}
		rows.Close()
		groups = append(groups, g)
	}
	realRows.Close()
	virtualRows, err := s.db.SQL.QueryContext(r.Context(), `SELECT g.id,g.name,coalesce(d.new_models_enabled,0) FROM virtual_provider_groups g LEFT JOIN client_group_defaults d ON d.client_key_id=? AND d.group_kind='virtual' AND d.group_id=g.id ORDER BY g.name`, clientID)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load permissions.")
		return
	}
	for virtualRows.Next() {
		var g permissionGroup
		var feeder int
		g.Kind = "virtual"
		_ = virtualRows.Scan(&g.ID, &g.Name, &feeder)
		g.NewModelsEnabled = scanBool(feeder)
		g.Models = []permissionModel{}
		rows, _ := s.db.SQL.QueryContext(r.Context(), `SELECT v.id,g.name||'/'||v.name,coalesce(x.enabled,0),(p.enabled=1 AND m.available=1) FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id JOIN providers p ON p.id=v.target_provider_id JOIN provider_models m ON m.id=v.target_provider_model_id LEFT JOIN client_model_permissions x ON x.client_key_id=? AND x.model_kind='virtual' AND x.model_id=v.id WHERE v.virtual_group_id=? ORDER BY v.name`, clientID, g.ID)
		for rows.Next() {
			var m permissionModel
			var enabled, available int
			m.Kind = "virtual"
			_ = rows.Scan(&m.ID, &m.CanonicalModelID, &enabled, &available)
			m.Enabled = scanBool(enabled)
			m.Available = scanBool(available)
			g.Models = append(g.Models, m)
		}
		rows.Close()
		groups = append(groups, g)
	}
	virtualRows.Close()
	writeJSON(w, 200, map[string]any{"client_key_id": clientID, "groups": groups, "feeder_explanation": "Controls whether models discovered or created in future are enabled for this client. Changing it never alters existing model permissions."})
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
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not update permissions.")
		return
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM client_keys WHERE id=?`, clientID).Scan(&exists); err != nil || exists == 0 {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	now := database.Now()
	for _, d := range input.Defaults {
		table := "providers"
		if d.Kind == "virtual" {
			table = "virtual_provider_groups"
		} else if d.Kind != "real" {
			adminError(w, 400, "invalid_permission", "Invalid group kind.")
			return
		}
		var valid int
		if tx.QueryRowContext(r.Context(), `SELECT count(*) FROM `+table+` WHERE id=?`, d.GroupID).Scan(&valid) != nil || valid == 0 {
			adminError(w, 400, "invalid_permission", "Unknown permission group.")
			return
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(client_key_id,group_kind,group_id) DO UPDATE SET new_models_enabled=excluded.new_models_enabled,updated_at=excluded.updated_at`, clientID, d.Kind, d.GroupID, boolInt(d.Enabled), now)
		if err != nil {
			adminError(w, 500, "database_error", "Could not update permissions.")
			return
		}
	}
	for _, p := range input.Permissions {
		table := "provider_models"
		if p.Kind == "virtual" {
			table = "virtual_models"
		} else if p.Kind != "real" {
			adminError(w, 400, "invalid_permission", "Invalid model kind.")
			return
		}
		var valid int
		if tx.QueryRowContext(r.Context(), `SELECT count(*) FROM `+table+` WHERE id=?`, p.ModelID).Scan(&valid) != nil || valid == 0 {
			adminError(w, 400, "invalid_permission", "Unknown model permission.")
			return
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(client_key_id,model_kind,model_id) DO UPDATE SET enabled=excluded.enabled,updated_at=excluded.updated_at`, clientID, p.Kind, p.ModelID, boolInt(p.Enabled), now, now)
		if err != nil {
			adminError(w, 500, "database_error", "Could not update permissions.")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		adminError(w, 500, "database_error", "Could not update permissions.")
		return
	}
	w.WriteHeader(204)
}
