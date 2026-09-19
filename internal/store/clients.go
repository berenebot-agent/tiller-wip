package store

import (
	"context"
	"database/sql"
	"errors"
)

// Sentinel errors returned by client/permission operations so the transport
// layer can map them to the right HTTP status. All of them mean "not visible
// to this account" as far as the caller is concerned.
var (
	ErrClientKeyNotFound       = errors.New("store: client key not found")
	ErrPermissionGroupNotFound = errors.New("store: permission group not found")
	ErrPermissionModelNotFound = errors.New("store: permission model not found")
	ErrSingleTargetNotFound    = errors.New("store: single-key target not found")
)

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// ClientKeyRow is one row of the client-key admin list, including the joined
// single-binding target summary.
type ClientKeyRow struct {
	ID                    string
	Name                  string
	Description           string
	Group                 string
	Fingerprint           string
	Enabled               bool
	LoggingEnabled        bool
	RetentionDays         int
	CreatedAt             string
	RotatedAt             sql.NullString
	UpdatedAt             string
	Type                  string
	SingleModelName       string
	SingleTargetType      string
	SingleTargetID        string
	SingleTargetCanonical string
	SingleTargetAvailable bool
}

// ClientKeyFilter narrows ListClientKeys.
type ClientKeyFilter struct {
	Search string
	Group  string
	Limit  int
	Offset int
}

// ListClientKeys returns the account's client keys matching the filter.
func (s *Scope) ListClientKeys(ctx context.Context, filter ClientKeyFilter) ([]ClientKeyRow, error) {
	pattern := "%" + filter.Search + "%"
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
WHERE (c.name LIKE ? OR c.description LIKE ? OR c.key_group LIKE ?) AND c.account_id=?`
	args := []any{pattern, pattern, pattern, s.accountID}
	if filter.Group != "" {
		query += ` AND c.key_group = ?`
		args = append(args, filter.Group)
	}
	query += ` ORDER BY c.name LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClientKeyRow{}
	for rows.Next() {
		var v ClientKeyRow
		var enabled, loggingEnabled, targetAvailable int
		if err := rows.Scan(&v.ID, &v.Name, &v.Description, &v.Group, &v.Fingerprint, &enabled, &loggingEnabled, &v.RetentionDays, &v.CreatedAt, &v.RotatedAt, &v.UpdatedAt, &v.Type, &v.SingleModelName, &v.SingleTargetType, &v.SingleTargetID, &v.SingleTargetCanonical, &targetAvailable); err != nil {
			return nil, err
		}
		v.Enabled = enabled != 0
		v.LoggingEnabled = loggingEnabled != 0
		v.SingleTargetAvailable = targetAvailable != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// ClientKeyAuthRow is the principal lookup row used by authentication.
type ClientKeyAuthRow struct {
	ID            string
	AccountID     string
	AccountStatus string
	Name          string
	Enabled       bool
	SecretHash    string
}

// ClientKeyBySelector looks up a client key by its public selector. It is the
// pre-account authentication path (the key determines the account), so it is
// deliberately platform-level rather than account-scoped.
func (s *Store) ClientKeyBySelector(ctx context.Context, selector string) (ClientKeyAuthRow, error) {
	var v ClientKeyAuthRow
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT c.id,c.account_id,a.status,c.name,c.enabled,c.secret_hash FROM client_keys c JOIN accounts a ON a.id=c.account_id WHERE c.selector=?`, selector).Scan(&v.ID, &v.AccountID, &v.AccountStatus, &v.Name, &enabled, &v.SecretHash)
	if err != nil {
		return ClientKeyAuthRow{}, err
	}
	v.Enabled = enabled != 0
	return v, nil
}

// UpdateClientKeySecretHash lazily migrates a client key's stored hash after a
// successful verify. The compare-and-swap keeps it safe against concurrent
// rotation.
func (s *Store) UpdateClientKeySecretHash(ctx context.Context, id, oldHash, newHash, updatedAt string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE client_keys SET secret_hash=?, updated_at=? WHERE id=? AND secret_hash=?`, newHash, updatedAt, id, oldHash)
	return err
}

// ClientKeyEditable is the mutable field set read before an update so the
// handler can merge a PATCH.
type ClientKeyEditable struct {
	Name           string
	Description    string
	Group          string
	Type           string
	Enabled        bool
	LoggingEnabled bool
	RetentionDays  int
}

func (s *Scope) GetClientKeyEditable(ctx context.Context, id string) (ClientKeyEditable, error) {
	var v ClientKeyEditable
	var enabled, loggingEnabled int
	err := s.q.QueryRowContext(ctx, `SELECT name,description,key_group,enabled,logging_enabled,retention_days,key_type FROM client_keys WHERE id=? AND account_id=?`, id, s.accountID).
		Scan(&v.Name, &v.Description, &v.Group, &enabled, &loggingEnabled, &v.RetentionDays, &v.Type)
	if errors.Is(err, sql.ErrNoRows) {
		return ClientKeyEditable{}, ErrClientKeyNotFound
	}
	if err != nil {
		return ClientKeyEditable{}, err
	}
	v.Enabled = enabled != 0
	v.LoggingEnabled = loggingEnabled != 0
	return v, nil
}

// ClientKeyType returns a client key's type ("catalogue"/"single").
func (s *Scope) ClientKeyType(ctx context.Context, id string) (string, error) {
	var keyType string
	err := s.q.QueryRowContext(ctx, `SELECT key_type FROM client_keys WHERE id=? AND account_id=?`, id, s.accountID).Scan(&keyType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrClientKeyNotFound
	}
	return keyType, err
}

// ClientKeyLoggingEnabled reports whether a client key should have its
// request activity logged.
func (s *Scope) ClientKeyLoggingEnabled(ctx context.Context, id string) (bool, error) {
	var enabled int
	err := s.q.QueryRowContext(ctx, `SELECT logging_enabled FROM client_keys WHERE id=? AND account_id=?`, id, s.accountID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrClientKeyNotFound
	}
	return enabled != 0, err
}

// ClientKeyName returns a client key's name, used for delete notifications.
func (s *Scope) ClientKeyName(ctx context.Context, id string) (string, error) {
	var name string
	err := s.q.QueryRowContext(ctx, `SELECT name FROM client_keys WHERE id=? AND account_id=?`, id, s.accountID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrClientKeyNotFound
	}
	return name, err
}

// SingleBindingRow is a client key's single-model binding.
type SingleBindingRow struct {
	ModelName      string
	RealModelID    sql.NullString
	VirtualModelID sql.NullString
}

// GetSingleBinding returns a client key's single-model binding. found is false
// when the key has no binding row.
func (s *Scope) GetSingleBinding(ctx context.Context, clientID string) (v SingleBindingRow, found bool, err error) {
	err = s.q.QueryRowContext(ctx, `SELECT exposed_model_name,real_model_id,virtual_model_id FROM client_single_bindings WHERE client_key_id=? AND account_id=?`, clientID, s.accountID).
		Scan(&v.ModelName, &v.RealModelID, &v.VirtualModelID)
	if errors.Is(err, sql.ErrNoRows) {
		return SingleBindingRow{}, false, nil
	}
	if err != nil {
		return SingleBindingRow{}, false, err
	}
	return v, true, nil
}

// singleTargetExists checks a Single-key target within the scope's account.
func (s *Scope) singleTargetExists(ctx context.Context, targetType, targetID string) (bool, error) {
	table := ""
	switch targetType {
	case "real":
		table = "provider_models"
	case "virtual":
		table = "virtual_models"
	default:
		return false, nil
	}
	var exists int
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE id=? AND account_id=?`, targetID, s.accountID).Scan(&exists); err != nil {
		return false, err
	}
	return exists == 1, nil
}

// CreateClientKeyInput carries the key material (generated by the caller) and
// the fields needed to persist a client key plus its initial permissions.
type CreateClientKeyInput struct {
	ID               string
	Name             string
	Description      string
	Group            string
	Selector         string
	Hash             string
	Fingerprint      string
	Type             string
	LoggingEnabled   bool
	RetentionDays    int
	SingleModelName  string
	SingleTargetType string
	SingleTargetID   string
}

// CreateClientKey persists a client key and seeds its group/model permissions
// from only the calling account's own providers, groups, models and routes.
func (s *Scope) CreateClientKey(ctx context.Context, in CreateClientKeyInput) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		if in.Type == "single" {
			ok, err := tx.singleTargetExists(ctx, in.SingleTargetType, in.SingleTargetID)
			if err != nil {
				return err
			}
			if !ok {
				return ErrSingleTargetNotFound
			}
		}
		now := now()
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_keys(id,account_id,name,description,key_group,selector,secret_hash,secret_fingerprint,enabled,logging_enabled,retention_days,created_at,updated_at,key_type) VALUES(?,?,?,?,?,?,?,?,1,?,?,?,?,?)`, in.ID, tx.accountID, in.Name, in.Description, in.Group, in.Selector, in.Hash, in.Fingerprint, boolInt(in.LoggingEnabled), in.RetentionDays, now, now, in.Type); err != nil {
			return err
		}
		if in.Type == "single" {
			if err := tx.upsertSingleBinding(ctx, in.ID, in.SingleModelName, in.SingleTargetType, in.SingleTargetID, now); err != nil {
				return err
			}
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at,account_id) SELECT ?,'real',id,0,?,? FROM providers WHERE account_id=?`, in.ID, now, tx.accountID, tx.accountID); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at,account_id) SELECT ?,'virtual',id,0,?,? FROM virtual_provider_groups WHERE account_id=?`, in.ID, now, tx.accountID, tx.accountID); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at,account_id) SELECT ?,'real',id,0,?,?,? FROM provider_models WHERE account_id=?`, in.ID, now, now, tx.accountID, tx.accountID); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at,account_id) SELECT ?,'virtual',id,0,?,?,? FROM virtual_models WHERE account_id=?`, in.ID, now, now, tx.accountID, tx.accountID); err != nil {
			return err
		}
		return nil
	})
}

func (s *Scope) upsertSingleBinding(ctx context.Context, clientID, modelName, targetType, targetID, now string) error {
	var realID, virtualID any
	if targetType == "real" {
		realID = targetID
	} else {
		virtualID = targetID
	}
	_, err := s.q.ExecContext(ctx, `INSERT INTO client_single_bindings(client_key_id,exposed_model_name,real_model_id,virtual_model_id,created_at,updated_at,account_id)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(client_key_id) DO UPDATE SET exposed_model_name=excluded.exposed_model_name,real_model_id=excluded.real_model_id,virtual_model_id=excluded.virtual_model_id,updated_at=excluded.updated_at`,
		clientID, modelName, realID, virtualID, now, now, s.accountID)
	return err
}

// UpdateClientKeyInput is the fully merged field set for an update.
type UpdateClientKeyInput struct {
	ID             string
	Name           string
	Description    string
	Group          string
	Type           string
	Enabled        bool
	LoggingEnabled bool
	RetentionDays  int
	WriteBinding   bool
	ModelName      string
	TargetType     string
	TargetID       string
}

// UpdateClientKey writes the merged client-key fields (and optionally the
// single binding) for the scoped account.
func (s *Scope) UpdateClientKey(ctx context.Context, in UpdateClientKeyInput) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		now := now()
		res, err := tx.q.ExecContext(ctx, `UPDATE client_keys SET name=?,description=?,key_group=?,enabled=?,logging_enabled=?,retention_days=?,key_type=?,updated_at=? WHERE id=? AND account_id=?`,
			in.Name, in.Description, in.Group, boolInt(in.Enabled), boolInt(in.LoggingEnabled), in.RetentionDays, in.Type, now, in.ID, tx.accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrClientKeyNotFound
		}
		if in.WriteBinding {
			ok, err := tx.singleTargetExists(ctx, in.TargetType, in.TargetID)
			if err != nil {
				return err
			}
			if !ok {
				return ErrSingleTargetNotFound
			}
			return tx.upsertSingleBinding(ctx, in.ID, in.ModelName, in.TargetType, in.TargetID, now)
		}
		return nil
	})
}

// RotateClientKey replaces a client key's selector and secret hash. It returns
// false when no row in the scope's account matched.
func (s *Scope) RotateClientKey(ctx context.Context, id, selector, hash, fingerprint string) (bool, error) {
	now := now()
	res, err := s.q.ExecContext(ctx, `UPDATE client_keys SET selector=?,secret_hash=?,secret_fingerprint=?,rotated_at=?,updated_at=? WHERE id=? AND account_id=?`, selector, hash, fingerprint, now, now, id, s.accountID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteClientKey removes a client key. It returns false when no row in the
// scope's account matched. Activity rows for the key are removed best-effort
// after the core delete: the Activity database is a separate file, so the old
// ON DELETE CASCADE cannot span it. A failed cleanup leaves harmless orphan
// rows that the retention sweep cannot attribute any more, so it is logged by
// the caller and retried on the next delete of the same key.
func (s *Scope) DeleteClientKey(ctx context.Context, id string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `DELETE FROM client_keys WHERE id=? AND account_id=?`, id, s.accountID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	// Best-effort: never fail the key deletion because Activity cleanup
	// failed. The rows are metadata only and are account-scoped.
	_ = s.withActivity(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `DELETE FROM request_logs WHERE client_key_id=? AND account_id=?`, id, s.accountID)
		return err
	})
	return true, nil
}

// PermissionModelRow is one model within a permission group.
type PermissionModelRow struct {
	Kind             string
	ID               string
	CanonicalModelID string
	Enabled          bool
	Available        bool
}

// PermissionGroupRow is one provider/virtual group with its per-model
// permissions for a client key.
type PermissionGroupRow struct {
	Kind             string
	ID               string
	Name             string
	NewModelsEnabled bool
	Models           []PermissionModelRow
}

// ListPermissions returns the account's permission matrix for a client key.
func (s *Scope) ListPermissions(ctx context.Context, clientID string) ([]PermissionGroupRow, error) {
	var exists int
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM client_keys WHERE id=? AND account_id=?`, clientID, s.accountID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, ErrClientKeyNotFound
	}
	groups := []PermissionGroupRow{}

	realRows, err := s.q.QueryContext(ctx, `SELECT p.id,p.name,coalesce(d.new_models_enabled,0) FROM providers p LEFT JOIN client_group_defaults d ON d.client_key_id=? AND d.group_kind='real' AND d.group_id=p.id AND d.account_id=? WHERE p.account_id=? ORDER BY p.name`, clientID, s.accountID, s.accountID)
	if err != nil {
		return nil, err
	}
	realGroups, err := scanPermissionGroups(ctx, s, realRows, "real")
	if err != nil {
		return nil, err
	}
	for i := range realGroups {
		rows, err := s.q.QueryContext(ctx, `SELECT m.id,p.name||'/'||m.upstream_model_id,coalesce(x.enabled,0),m.available FROM provider_models m JOIN providers p ON p.id=m.provider_id LEFT JOIN client_model_permissions x ON x.client_key_id=? AND x.model_kind='real' AND x.model_id=m.id AND x.account_id=? WHERE m.provider_id=? AND m.account_id=? AND p.account_id=? ORDER BY m.upstream_model_id`, clientID, s.accountID, realGroups[i].ID, s.accountID, s.accountID)
		if err != nil {
			return nil, err
		}
		if err := scanPermissionModels(rows, "real", &realGroups[i]); err != nil {
			return nil, err
		}
	}
	groups = append(groups, realGroups...)

	virtualRows, err := s.q.QueryContext(ctx, `SELECT g.id,g.name,coalesce(d.new_models_enabled,0) FROM virtual_provider_groups g LEFT JOIN client_group_defaults d ON d.client_key_id=? AND d.group_kind='virtual' AND d.group_id=g.id AND d.account_id=? WHERE g.account_id=? ORDER BY g.name`, clientID, s.accountID, s.accountID)
	if err != nil {
		return nil, err
	}
	virtualGroups, err := scanPermissionGroups(ctx, s, virtualRows, "virtual")
	if err != nil {
		return nil, err
	}
	for i := range virtualGroups {
		rows, err := s.q.QueryContext(ctx, `SELECT v.id,g.name||'/'||v.name,coalesce(x.enabled,0),EXISTS(SELECT 1 FROM virtual_model_targets t JOIN provider_models m2 ON m2.id=t.provider_model_id JOIN providers p2 ON p2.id=m2.provider_id WHERE t.virtual_model_id=v.id AND t.enabled=1 AND m2.available=1 AND p2.enabled=1) FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id LEFT JOIN client_model_permissions x ON x.client_key_id=? AND x.model_kind='virtual' AND x.model_id=v.id AND x.account_id=? WHERE v.virtual_group_id=? AND v.account_id=? AND g.account_id=? ORDER BY v.name`, clientID, s.accountID, virtualGroups[i].ID, s.accountID, s.accountID)
		if err != nil {
			return nil, err
		}
		if err := scanPermissionModels(rows, "virtual", &virtualGroups[i]); err != nil {
			return nil, err
		}
	}
	groups = append(groups, virtualGroups...)
	return groups, nil
}

func scanPermissionGroups(ctx context.Context, s *Scope, rows *sql.Rows, kind string) ([]PermissionGroupRow, error) {
	defer rows.Close()
	out := []PermissionGroupRow{}
	for rows.Next() {
		g := PermissionGroupRow{Kind: kind, Models: []PermissionModelRow{}}
		var feeder int
		if err := rows.Scan(&g.ID, &g.Name, &feeder); err != nil {
			return nil, err
		}
		g.NewModelsEnabled = feeder != 0
		out = append(out, g)
	}
	return out, rows.Err()
}

func scanPermissionModels(rows *sql.Rows, kind string, g *PermissionGroupRow) error {
	defer rows.Close()
	for rows.Next() {
		m := PermissionModelRow{Kind: kind}
		var enabled, available int
		if err := rows.Scan(&m.ID, &m.CanonicalModelID, &enabled, &available); err != nil {
			return err
		}
		m.Enabled = enabled != 0
		m.Available = available != 0
		g.Models = append(g.Models, m)
	}
	return rows.Err()
}

// PermissionDefaultUpdate toggles the feeder default for a group.
type PermissionDefaultUpdate struct {
	Kind    string
	GroupID string
	Enabled bool
}

// PermissionModelUpdate toggles one model permission.
type PermissionModelUpdate struct {
	Kind    string
	ModelID string
	Enabled bool
}

// UpdatePermissions applies a batch of feeder-default and model-permission
// upserts for the account's client key.
func (s *Scope) UpdatePermissions(ctx context.Context, clientID string, defaults []PermissionDefaultUpdate, permissions []PermissionModelUpdate) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		var exists int
		if err := tx.q.QueryRowContext(ctx, `SELECT count(*) FROM client_keys WHERE id=? AND account_id=?`, clientID, tx.accountID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrClientKeyNotFound
		}
		now := now()
		for _, d := range defaults {
			table := ""
			switch d.Kind {
			case "real":
				table = "providers"
			case "virtual":
				table = "virtual_provider_groups"
			default:
				return ErrPermissionGroupNotFound
			}
			var valid int
			if err := tx.q.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE id=? AND account_id=?`, d.GroupID, tx.accountID).Scan(&valid); err != nil {
				return err
			}
			if valid == 0 {
				return ErrPermissionGroupNotFound
			}
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at,account_id) VALUES(?,?,?,?,?,?) ON CONFLICT(client_key_id,group_kind,group_id) DO UPDATE SET new_models_enabled=excluded.new_models_enabled,updated_at=excluded.updated_at`, clientID, d.Kind, d.GroupID, boolInt(d.Enabled), now, tx.accountID); err != nil {
				return err
			}
		}
		for _, p := range permissions {
			table := ""
			switch p.Kind {
			case "real":
				table = "provider_models"
			case "virtual":
				table = "virtual_models"
			default:
				return ErrPermissionModelNotFound
			}
			var valid int
			if err := tx.q.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE id=? AND account_id=?`, p.ModelID, tx.accountID).Scan(&valid); err != nil {
				return err
			}
			if valid == 0 {
				return ErrPermissionModelNotFound
			}
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at,account_id) VALUES(?,?,?,?,?,?,?) ON CONFLICT(client_key_id,model_kind,model_id) DO UPDATE SET enabled=excluded.enabled,updated_at=excluded.updated_at`, clientID, p.Kind, p.ModelID, boolInt(p.Enabled), now, now, tx.accountID); err != nil {
				return err
			}
		}
		return nil
	})
}
