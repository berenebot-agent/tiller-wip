package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
)

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

func validateSingleTarget(tx *sql.Tx, targetType, targetID string) error {
	table := "provider_models"
	if targetType == "virtual" {
		table = "virtual_models"
	} else if targetType != "real" {
		return sql.ErrNoRows
	}
	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM `+table+` WHERE id=?`, targetID).Scan(&exists); err != nil || exists != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func upsertSingleBinding(tx *sql.Tx, clientID, modelName, targetType, targetID, now string) error {
	var realID, virtualID any
	if targetType == "real" {
		realID = targetID
	} else {
		virtualID = targetID
	}
	_, err := tx.Exec(`INSERT INTO client_single_bindings(client_key_id,exposed_model_name,real_model_id,virtual_model_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(client_key_id) DO UPDATE SET exposed_model_name=excluded.exposed_model_name,real_model_id=excluded.real_model_id,virtual_model_id=excluded.virtual_model_id,updated_at=excluded.updated_at`, clientID, modelName, realID, virtualID, now, now)
	return err
}

func (s *Server) listClientKeys(w http.ResponseWriter, r *http.Request) {
	limit, offset, search := pagination(r)
	groupFilter := strings.TrimSpace(r.URL.Query().Get("group"))
	pattern := "%" + search + "%"
	query := `SELECT c.id,c.name,c.description,c.key_group,c.secret_fingerprint,c.enabled,c.logging_enabled,c.retention_days,c.created_at,c.rotated_at,c.updated_at,c.key_type,
		coalesce(b.exposed_model_name,''),
		CASE WHEN b.real_model_id IS NOT NULL THEN 'real' WHEN b.virtual_model_id IS NOT NULL THEN 'virtual' ELSE '' END,
		coalesce(b.real_model_id,b.virtual_model_id,''),
		CASE WHEN b.real_model_id IS NOT NULL THEN coalesce(rp.name||'/'||rm.upstream_model_id,'') WHEN b.virtual_model_id IS NOT NULL THEN coalesce(vg.name||'/'||vm.name,'') ELSE '' END,
		CASE WHEN b.real_model_id IS NOT NULL THEN coalesce(rp.enabled=1 AND rm.available=1,0)
		     WHEN b.virtual_model_id IS NOT NULL THEN EXISTS(SELECT 1 FROM virtual_model_targets vt JOIN provider_models pm ON pm.id=vt.provider_model_id JOIN providers p ON p.id=pm.provider_id WHERE vt.virtual_model_id=b.virtual_model_id AND vt.enabled=1 AND pm.available=1 AND p.enabled=1)
		     ELSE 0 END
		FROM client_keys c LEFT JOIN client_single_bindings b ON b.client_key_id=c.id
		LEFT JOIN provider_models rm ON rm.id=b.real_model_id LEFT JOIN providers rp ON rp.id=rm.provider_id
		LEFT JOIN virtual_models vm ON vm.id=b.virtual_model_id LEFT JOIN virtual_provider_groups vg ON vg.id=vm.virtual_group_id
		WHERE (c.name LIKE ? OR c.description LIKE ? OR c.key_group LIKE ?)`
	args := []any{pattern, pattern, pattern}
	if groupFilter != "" {
		query += ` AND c.key_group = ?`
		args = append(args, groupFilter)
	}
	query += ` ORDER BY c.name LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.SQL.QueryContext(r.Context(), query, args...)
	if err != nil {
		adminError(w, 500, "database_error", "Could not list client keys.")
		return
	}
	defer rows.Close()
	data := []clientKeyView{}
	for rows.Next() {
		var v clientKeyView
		var enabled, loggingEnabled int
		var targetAvailable int
		if rows.Scan(&v.ID, &v.Name, &v.Description, &v.Group, &v.Fingerprint, &enabled, &loggingEnabled, &v.RetentionDays, &v.CreatedAt, &v.RotatedAt, &v.UpdatedAt, &v.Type, &v.SingleModelName, &v.SingleTargetType, &v.SingleTargetID, &v.SingleTargetCanonical, &targetAvailable) != nil {
			adminError(w, 500, "database_error", "Could not list client keys.")
			return
		}
		v.Enabled = scanBool(enabled)
		v.LoggingEnabled = scanBool(loggingEnabled)
		v.SingleTargetAvailable = scanBool(targetAvailable)
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
	loggingEnabled, retentionDays, err := s.db.GetLoggingDefaults(r.Context())
	if err != nil {
		adminError(w, 500, "database_error", "Could not create client key.")
		return
	}
	now := database.Now()
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not create client key.")
		return
	}
	defer tx.Rollback()
	if input.Type == "single" {
		if err = validateSingleTarget(tx, input.SingleTargetType, input.SingleTargetID); err != nil {
			adminError(w, 400, "invalid_target", "The selected Single-key target does not exist.")
			return
		}
	}
	_, err = tx.ExecContext(r.Context(), `INSERT INTO client_keys(id,name,description,key_group,selector,secret_hash,secret_fingerprint,enabled,logging_enabled,retention_days,created_at,updated_at,key_type) VALUES(?,?,?,?,?,?,?,1,?,?,?,?,?)`, clientID, input.Name, input.Description, group, generated.Selector, generated.Hash, generated.Fingerprint, boolInt(loggingEnabled), retentionDays, now, now, input.Type)
	if err == nil && input.Type == "single" {
		err = upsertSingleBinding(tx, clientID, input.SingleModelName, input.SingleTargetType, input.SingleTargetID, now)
	}
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
	writeJSON(w, 201, map[string]any{"id": clientID, "name": input.Name, "type": input.Type, "secret": generated.Plaintext, "fingerprint": generated.Fingerprint, "warning": "Copy this key now. It cannot be displayed again."})
	s.notifyAdminEvent(eventClientKeyCreated, fmt.Sprintf("Client: %s\nType: %s", input.Name, input.Type))
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
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		adminError(w, 500, "database_error", "Could not update client key.")
		return
	}
	defer tx.Rollback()
	var name, description, keyType, keyGroup string
	var enabled, loggingEnabled, retentionDays int
	if err = tx.QueryRowContext(r.Context(), `SELECT name,description,key_group,enabled,logging_enabled,retention_days,key_type FROM client_keys WHERE id=?`, clientID).Scan(&name, &description, &keyGroup, &enabled, &loggingEnabled, &retentionDays, &keyType); err == sql.ErrNoRows {
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
	if input.Group != nil {
		keyGroup = normalizeClientKeyGroup(*input.Group)
		if !clientKeyGroupPattern.MatchString(keyGroup) {
			adminError(w, 400, "invalid_group", "Group names must be 1-63 characters using letters, digits, spaces, dots, dashes, or underscores.")
			return
		}
	}
	if input.Enabled != nil {
		enabled = boolInt(*input.Enabled)
	}
	if input.LoggingEnabled != nil {
		loggingEnabled = boolInt(*input.LoggingEnabled)
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
	var oldModelName, targetType, targetID string
	var realID, virtualID sql.NullString
	bindErr := tx.QueryRowContext(r.Context(), `SELECT exposed_model_name,real_model_id,virtual_model_id FROM client_single_bindings WHERE client_key_id=?`, clientID).Scan(&oldModelName, &realID, &virtualID)
	if bindErr != nil && bindErr != sql.ErrNoRows {
		adminError(w, 500, "database_error", "Could not update client key.")
		return
	}
	modelName := oldModelName
	if bindErr == sql.ErrNoRows {
		modelName = "main"
	}
	if realID.Valid {
		targetType, targetID = "real", realID.String
	} else if virtualID.Valid {
		targetType, targetID = "virtual", virtualID.String
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
	if keyType == "single" || bindingSupplied {
		if !validClientModelName(modelName) {
			adminError(w, 400, "invalid_model_name", "Client-facing model names must use 1-255 model-safe characters.")
			return
		}
		if err = validateSingleTarget(tx, targetType, targetID); err != nil {
			adminError(w, 400, "invalid_target", "The selected Single-key target does not exist.")
			return
		}
		if oldType == "single" && bindErr == nil && modelName != oldModelName && !input.ConfirmModelNameChange {
			adminError(w, 409, "breaking_change_confirmation_required", "Changing the client-facing model name may require client reconfiguration. Confirm the breaking change.")
			return
		}
	}
	now := database.Now()
	_, err = tx.ExecContext(r.Context(), `UPDATE client_keys SET name=?,description=?,key_group=?,enabled=?,logging_enabled=?,retention_days=?,key_type=?,updated_at=? WHERE id=?`, name, description, keyGroup, enabled, loggingEnabled, retentionDays, keyType, now, clientID)
	if err == nil && (keyType == "single" || bindingSupplied) {
		err = upsertSingleBinding(tx, clientID, modelName, targetType, targetID, now)
	}
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
	var name string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT name FROM client_keys WHERE id=?`, clientID).Scan(&name); err == sql.ErrNoRows {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	} else if err != nil {
		adminError(w, 500, "database_error", "Could not delete client key.")
		return
	}
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
	s.notifyAdminEvent(eventClientKeyDeleted, fmt.Sprintf("Client: %s", name))
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
