package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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

// Detailed error body logging modes for a client key. "inherit" follows the
// account-level log_error_bodies setting, "always" captures failed request and
// provider error bodies regardless of that setting, and "never" never captures
// them.
const (
	BodyLogInherit = "inherit"
	BodyLogAlways  = "always"
	BodyLogNever   = "never"
)

// bodyLogMode maps the nullable column to its API value.
func bodyLogMode(v sql.NullBool) string {
	if !v.Valid {
		return BodyLogInherit
	}
	if v.Bool {
		return BodyLogAlways
	}
	return BodyLogNever
}

// bodyLogValue maps an API value to the nullable column value.
func bodyLogValue(mode string) any {
	switch mode {
	case BodyLogAlways:
		return 1
	case BodyLogNever:
		return 0
	default:
		return nil
	}
}

// ValidBodyLogMode reports whether mode is a known body-logging mode.
func ValidBodyLogMode(mode string) bool {
	switch mode {
	case BodyLogInherit, BodyLogAlways, BodyLogNever:
		return true
	}
	return false
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
	LogErrorBodies        string
	RetentionDays         int
	CreatedAt             string
	RotatedAt             sql.NullString
	UpdatedAt             string
	Type                  string
	SingleModelName       string
	SingleTargetType      string
	SingleTargetID        string
	SingleTargetCanonical string
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
	query := `SELECT c.id,c.name,c.description,c.key_group,c.secret_fingerprint,c.enabled,c.logging_enabled,c.log_error_bodies,c.retention_days,c.created_at,c.rotated_at,c.updated_at,c.key_type,
coalesce(b.exposed_model_name,''),
CASE WHEN b.real_model_id IS NOT NULL THEN 'real' WHEN b.virtual_model_id IS NOT NULL THEN 'virtual' ELSE '' END,
coalesce(b.real_model_id,b.virtual_model_id,''),
CASE WHEN b.real_model_id IS NOT NULL THEN coalesce(rp.name||'/'||rm.upstream_model_id,'') WHEN b.virtual_model_id IS NOT NULL THEN coalesce(vg.name||'/'||vm.name,'') ELSE '' END
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
		var enabled, loggingEnabled int
		var bodyLog sql.NullBool
		if err := rows.Scan(&v.ID, &v.Name, &v.Description, &v.Group, &v.Fingerprint, &enabled, &loggingEnabled, &bodyLog, &v.RetentionDays, &v.CreatedAt, &v.RotatedAt, &v.UpdatedAt, &v.Type, &v.SingleModelName, &v.SingleTargetType, &v.SingleTargetID, &v.SingleTargetCanonical); err != nil {
			return nil, err
		}
		v.Enabled = enabled != 0
		v.LoggingEnabled = loggingEnabled != 0
		v.LogErrorBodies = bodyLogMode(bodyLog)
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
	LogErrorBodies string
	RetentionDays  int
}

func (s *Scope) GetClientKeyEditable(ctx context.Context, id string) (ClientKeyEditable, error) {
	var v ClientKeyEditable
	var enabled, loggingEnabled int
	var bodyLog sql.NullBool
	err := s.q.QueryRowContext(ctx, `SELECT name,description,key_group,enabled,logging_enabled,log_error_bodies,retention_days,key_type FROM client_keys WHERE id=? AND account_id=?`, id, s.accountID).
		Scan(&v.Name, &v.Description, &v.Group, &enabled, &loggingEnabled, &bodyLog, &v.RetentionDays, &v.Type)
	if errors.Is(err, sql.ErrNoRows) {
		return ClientKeyEditable{}, ErrClientKeyNotFound
	}
	if err != nil {
		return ClientKeyEditable{}, err
	}
	v.Enabled = enabled != 0
	v.LoggingEnabled = loggingEnabled != 0
	v.LogErrorBodies = bodyLogMode(bodyLog)
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

// ClientKeyLoggingFlags returns the client key's request-logging switch and
// its detailed-error-body mode in one read. The body mode falls back to
// "never" on error, so a failed read can never widen what is captured.
func (s *Scope) ClientKeyLoggingFlags(ctx context.Context, id string) (bool, string, error) {
	var enabled int
	var bodyLog sql.NullBool
	err := s.q.QueryRowContext(ctx, `SELECT logging_enabled,log_error_bodies FROM client_keys WHERE id=? AND account_id=?`, id, s.accountID).Scan(&enabled, &bodyLog)
	if errors.Is(err, sql.ErrNoRows) {
		return false, BodyLogNever, ErrClientKeyNotFound
	}
	if err != nil {
		return false, BodyLogNever, err
	}
	return enabled != 0, bodyLogMode(bodyLog), nil
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

// ClientKeyNames returns id→name for every client key in the scope's account.
// It lets the usage envelope label its client-keyed aggregates (client_keys,
// client_cache, client_cost, client_tokens) without a per-row join. A usage
// entry whose id is absent here belongs to a client key that has since been
// deleted (Activity history is retained after key deletion by design).
func (s *Scope) ClientKeyNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, name FROM client_keys WHERE account_id=?`, s.accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
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
		if tx.enforceLimits {
			if err := tx.EnforceClientKeyLimit(ctx); err != nil {
				return err
			}
		}
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

// UpdateClientKeyInput is the update request. Only fields whose *Set flag is
// true are written, so a request that omitted a field cannot overwrite a
// concurrent change to it with a value merged from an earlier read.
type UpdateClientKeyInput struct {
	ID          string
	Name        string
	NameSet     bool
	Description string
	DescSet     bool
	Group       string
	GroupSet    bool
	Type        string
	TypeSet     bool
	Enabled     bool
	EnabledSet  bool
	Logging     bool
	LoggingSet  bool
	// LogErrorBodies is the client key's detailed-error-body mode
	// (inherit/always/never). An empty string leaves the mode untouched.
	LogErrorBodies string
	Retention      int
	RetentionSet   bool
	// BindingSet reports whether the request supplied Single-binding fields. The
	// binding is only written when it is set (or when a Single key needs a
	// binding that does not exist yet), so a metadata-only PATCH cannot revert a
	// concurrent route change with a stale binding.
	BindingSet bool
	// BindingExists reports whether this key already has a Single binding, so
	// the store can distinguish "create the binding a Single key needs" from
	// "rewrite an existing binding the request did not mention".
	BindingExists bool
	ModelName     string
	TargetType    string
	TargetID      string
}

// UpdateClientKey writes only the supplied client-key fields, and the Single
// binding only when the request actually touched it (or the key is Single and
// has no binding yet), for the scoped account.
func (s *Scope) UpdateClientKey(ctx context.Context, in UpdateClientKeyInput) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		now := now()
		// updated_at is always written so a no-op update still affects one row
		// and the RowsAffected()==0 → not-found signal stays correct.
		set := "updated_at=?"
		args := []any{now}
		if in.NameSet {
			set += ",name=?"
			args = append(args, in.Name)
		}
		if in.DescSet {
			set += ",description=?"
			args = append(args, in.Description)
		}
		if in.GroupSet {
			set += ",key_group=?"
			args = append(args, in.Group)
		}
		if in.LoggingSet {
			set += ",logging_enabled=?"
			args = append(args, boolInt(in.Logging))
		}
		if in.LogErrorBodies != "" {
			set += ",log_error_bodies=?"
			args = append(args, bodyLogValue(in.LogErrorBodies))
		}
		if in.RetentionSet {
			set += ",retention_days=?"
			args = append(args, in.Retention)
		}
		if in.TypeSet {
			set += ",key_type=?"
			args = append(args, in.Type)
		}
		if in.EnabledSet {
			set += ",enabled=?"
			args = append(args, boolInt(in.Enabled))
		}
		args = append(args, in.ID, tx.accountID)
		res, err := tx.q.ExecContext(ctx, `UPDATE client_keys SET `+set+` WHERE id=? AND account_id=?`, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrClientKeyNotFound
		}
		writeBinding := in.BindingSet || (in.TypeSet && in.Type == "single" && !in.BindingExists)
		if writeBinding {
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

// DeleteClientKey removes a client key and records Activity cleanup before
// attempting the separate-file delete. The record also makes retries safe when
// the core key has already gone.
func (s *Scope) DeleteClientKey(ctx context.Context, id string) (bool, error) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	var exists int
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM client_keys WHERE id=? AND account_id=?`, id, s.accountID).Scan(&exists); err != nil {
		return false, err
	}
	var pending int
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM activity_cleanup WHERE account_id=? AND client_key_id=?`, s.accountID, id).Scan(&pending); err != nil {
		return false, err
	}
	if exists == 0 && pending == 0 {
		return false, nil
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO activity_cleanup(account_id,client_key_id,created_at) VALUES(?,?,?) ON CONFLICT(account_id,client_key_id) DO NOTHING`, s.accountID, id, now()); err != nil {
		return false, err
	}
	if exists == 0 {
		return true, nil
	}
	res, err := s.q.ExecContext(ctx, `DELETE FROM client_keys WHERE id=? AND account_id=?`, id, s.accountID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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
	realGroups, err := scanPermissionGroups(realRows, "real")
	if err != nil {
		return nil, err
	}
	if len(realGroups) > 0 {
		// One query for every group on the page, not one query per group.
		placeholders, args := permissionGroupArgs(clientID, s.accountID, realGroups)
		modelRows, err := s.q.QueryContext(ctx, `SELECT m.provider_id,m.id,p.name||'/'||m.upstream_model_id,coalesce(x.enabled,0),m.available FROM provider_models m JOIN providers p ON p.id=m.provider_id LEFT JOIN client_model_permissions x ON x.client_key_id=? AND x.model_kind='real' AND x.model_id=m.id AND x.account_id=? WHERE m.provider_id IN (`+placeholders+`) AND m.account_id=? AND p.account_id=? ORDER BY m.upstream_model_id`, args...)
		if err != nil {
			return nil, err
		}
		if err := scanPermissionModels(modelRows, "real", realGroups); err != nil {
			return nil, err
		}
	}
	groups = append(groups, realGroups...)

	virtualRows, err := s.q.QueryContext(ctx, `SELECT g.id,g.name,coalesce(d.new_models_enabled,0) FROM virtual_provider_groups g LEFT JOIN client_group_defaults d ON d.client_key_id=? AND d.group_kind='virtual' AND d.group_id=g.id AND d.account_id=? WHERE g.account_id=? ORDER BY g.name`, clientID, s.accountID, s.accountID)
	if err != nil {
		return nil, err
	}
	virtualGroups, err := scanPermissionGroups(virtualRows, "virtual")
	if err != nil {
		return nil, err
	}
	if len(virtualGroups) > 0 {
		placeholders, args := permissionGroupArgs(clientID, s.accountID, virtualGroups)
		modelRows, err := s.q.QueryContext(ctx, `SELECT v.virtual_group_id,v.id,g.name||'/'||v.name,coalesce(x.enabled,0),EXISTS(SELECT 1 FROM virtual_model_targets t JOIN provider_models m2 ON m2.id=t.provider_model_id JOIN providers p2 ON p2.id=m2.provider_id WHERE t.virtual_model_id=v.id AND t.enabled=1 AND m2.available=1 AND p2.enabled=1) FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id LEFT JOIN client_model_permissions x ON x.client_key_id=? AND x.model_kind='virtual' AND x.model_id=v.id AND x.account_id=? WHERE v.virtual_group_id IN (`+placeholders+`) AND v.account_id=? AND g.account_id=? ORDER BY v.name`, args...)
		if err != nil {
			return nil, err
		}
		if err := scanPermissionModels(modelRows, "virtual", virtualGroups); err != nil {
			return nil, err
		}
	}
	groups = append(groups, virtualGroups...)
	return groups, nil
}

// permissionGroupArgs builds the IN-list placeholders and the shared argument
// prefix for the batched permission-model queries: the client/account join
// keys, then the group ids, then the trailing account guards.
func permissionGroupArgs(clientID, accountID string, groups []PermissionGroupRow) (string, []any) {
	placeholders := make([]string, 0, len(groups))
	args := make([]any, 0, len(groups)+4)
	args = append(args, clientID, accountID)
	for i := range groups {
		placeholders = append(placeholders, "?")
		args = append(args, groups[i].ID)
	}
	args = append(args, accountID, accountID)
	return strings.Join(placeholders, ","), args
}

func scanPermissionGroups(rows *sql.Rows, kind string) ([]PermissionGroupRow, error) {
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

// scanPermissionModels appends each model row to its group. The first column of
// every row is the owning group id.
func scanPermissionModels(rows *sql.Rows, kind string, groups []PermissionGroupRow) error {
	defer rows.Close()
	byGroup := make(map[string]*PermissionGroupRow, len(groups))
	for i := range groups {
		byGroup[groups[i].ID] = &groups[i]
	}
	for rows.Next() {
		var groupID string
		m := PermissionModelRow{Kind: kind}
		var enabled, available int
		if err := rows.Scan(&groupID, &m.ID, &m.CanonicalModelID, &enabled, &available); err != nil {
			return err
		}
		m.Enabled = enabled != 0
		m.Available = available != 0
		if g := byGroup[groupID]; g != nil {
			g.Models = append(g.Models, m)
		}
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
