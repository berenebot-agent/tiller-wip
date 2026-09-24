package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/tiller-router/tiller-router/internal/id"
)

var (
	ErrVirtualGroupNotFound  = errors.New("store: virtual group not found")
	ErrVirtualGroupNotEmpty  = errors.New("store: virtual group has models")
	ErrVirtualModelNotFound  = errors.New("store: virtual model not found")
	ErrVirtualTargetNotFound = errors.New("store: virtual target model not found")
	ErrVirtualBindingInUse   = errors.New("store: virtual model referenced by a single client key")
)

// VirtualGroupRow is one row of the admin virtual-group list.
type VirtualGroupRow struct {
	ID         string
	Name       string
	CreatedAt  string
	UpdatedAt  string
	ModelCount int
}

// ListFilter narrows paginated virtual listings.
type ListFilter struct {
	Search string
	Limit  int
	Offset int
}

func (s *Scope) ListVirtualGroups(ctx context.Context, filter ListFilter) ([]VirtualGroupRow, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT g.id,g.name,g.created_at,g.updated_at,count(v.id) FROM virtual_provider_groups g LEFT JOIN virtual_models v ON v.virtual_group_id=g.id WHERE g.name LIKE ? AND g.account_id=? GROUP BY g.id ORDER BY g.name LIMIT ? OFFSET ?`, "%"+filter.Search+"%", s.accountID, filter.Limit, filter.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VirtualGroupRow{}
	for rows.Next() {
		var v VirtualGroupRow
		if err := rows.Scan(&v.ID, &v.Name, &v.CreatedAt, &v.UpdatedAt, &v.ModelCount); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CreateVirtualGroup inserts the namespace, group and default permissions.
func (s *Scope) CreateVirtualGroup(ctx context.Context, id, name string) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		now := now()
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO namespaces(account_id,name,kind,entity_id) VALUES(?,?,'virtual',?)`, tx.accountID, name, id); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO virtual_provider_groups(id,account_id,name,created_at,updated_at) VALUES(?,?,?,?,?)`, id, tx.accountID, name, now, now); err != nil {
			return err
		}
		_, err := tx.q.ExecContext(ctx, `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at,account_id) SELECT id,'virtual',?,0,?,? FROM client_keys WHERE account_id=?`, id, now, tx.accountID, tx.accountID)
		return err
	})
}

func (s *Scope) VirtualGroupName(ctx context.Context, id string) (string, error) {
	var name string
	err := s.q.QueryRowContext(ctx, `SELECT name FROM namespaces WHERE entity_id=? AND kind='virtual' AND account_id=?`, id, s.accountID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrVirtualGroupNotFound
	}
	return name, err
}

// UpdateVirtualGroup renames a group (and its namespace) within the account.
func (s *Scope) UpdateVirtualGroup(ctx context.Context, id, name string) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		res, err := tx.q.ExecContext(ctx, `UPDATE namespaces SET name=? WHERE entity_id=? AND kind='virtual' AND account_id=?`, name, id, tx.accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrVirtualGroupNotFound
		}
		if _, err := tx.q.ExecContext(ctx, `UPDATE virtual_provider_groups SET updated_at=? WHERE id=? AND account_id=?`, now(), id, tx.accountID); err != nil {
			return err
		}
		return nil
	})
}

// DeleteVirtualGroup removes an empty group. It returns ErrVirtualGroupNotEmpty
// when the group still owns virtual models.
func (s *Scope) DeleteVirtualGroup(ctx context.Context, id string) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		var count int
		if err := tx.q.QueryRowContext(ctx, `SELECT count(*) FROM virtual_models WHERE virtual_group_id=? AND account_id=?`, id, tx.accountID).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return ErrVirtualGroupNotEmpty
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM client_group_defaults WHERE group_kind='virtual' AND group_id=? AND account_id=?`, id, tx.accountID); err != nil {
			return err
		}
		res, err := tx.q.ExecContext(ctx, `DELETE FROM virtual_provider_groups WHERE id=? AND account_id=?`, id, tx.accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrVirtualGroupNotFound
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM namespaces WHERE entity_id=? AND kind='virtual' AND account_id=?`, id, tx.accountID); err != nil {
			return err
		}
		return nil
	})
}

// VirtualModelRow is one row of the admin virtual-model list.
type VirtualModelRow struct {
	ID               string
	GroupID          string
	GroupName        string
	Name             string
	CanonicalModelID string
	RoutingMode      string
	CreatedAt        string
	UpdatedAt        string
}

func (s *Scope) ListVirtualModels(ctx context.Context, filter ListFilter) ([]VirtualModelRow, error) {
	pattern := "%" + filter.Search + "%"
	rows, err := s.q.QueryContext(ctx, `SELECT v.id,g.id,g.name,v.name,g.name||'/'||v.name,v.routing_mode,v.created_at,v.updated_at FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE v.account_id=? AND g.account_id=? AND (g.name LIKE ? OR v.name LIKE ? OR g.name||'/'||v.name LIKE ? OR EXISTS(SELECT 1 FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=v.id AND t.account_id=? AND m.account_id=? AND p.account_id=? AND (p.name LIKE ? OR m.upstream_model_id LIKE ? OR p.name||'/'||m.upstream_model_id LIKE ?))) ORDER BY g.name,v.name LIMIT ? OFFSET ?`, s.accountID, s.accountID, pattern, pattern, pattern, s.accountID, s.accountID, s.accountID, pattern, pattern, pattern, filter.Limit, filter.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VirtualModelRow{}
	for rows.Next() {
		var v VirtualModelRow
		if err := rows.Scan(&v.ID, &v.GroupID, &v.GroupName, &v.Name, &v.CanonicalModelID, &v.RoutingMode, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VirtualTargetRow is one ordered target of a virtual model.
type VirtualTargetRow struct {
	ID                       string
	ProviderModelID          string
	ProviderID               string
	ProviderName             string
	UpstreamModelID          string
	NativeProtocol           string
	Position                 int
	Enabled                  bool
	Available                bool
	Warning                  string
	ContextLength            *int64
	MaxOutputTokens          *int64
	SupportsTools            sql.NullInt64
	SupportsVision           sql.NullInt64
	SupportsReasoning        sql.NullInt64
	SupportsStructuredOutput sql.NullInt64
	ReasoningCapabilities    sql.NullString
	InputModalities          sql.NullString
	OutputModalities         sql.NullString
}

func (s *Scope) VirtualTargets(ctx context.Context, virtualID string) ([]VirtualTargetRow, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT t.id,t.provider_model_id,p.id,p.name,m.upstream_model_id,coalesce(m.native_protocol,''),t.position,t.enabled,(p.enabled=1 AND m.available=1),CASE WHEN t.enabled=0 THEN 'Target is disabled' WHEN p.enabled=0 THEN 'Target provider is disabled' WHEN m.available=0 THEN 'Target model is retired' ELSE '' END,m.context_length,m.max_output_tokens,m.supports_tools,m.supports_vision,m.supports_reasoning,m.supports_structured_output,m.reasoning_capabilities,m.input_modalities,m.output_modalities FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=? AND t.account_id=? AND m.account_id=? AND p.account_id=? ORDER BY t.position`, virtualID, s.accountID, s.accountID, s.accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VirtualTargetRow{}
	for rows.Next() {
		var v VirtualTargetRow
		var enabled, available int
		if err := rows.Scan(&v.ID, &v.ProviderModelID, &v.ProviderID, &v.ProviderName, &v.UpstreamModelID, &v.NativeProtocol, &v.Position, &enabled, &available, &v.Warning, &v.ContextLength, &v.MaxOutputTokens, &v.SupportsTools, &v.SupportsVision, &v.SupportsReasoning, &v.SupportsStructuredOutput, &v.ReasoningCapabilities, &v.InputModalities, &v.OutputModalities); err != nil {
			return nil, err
		}
		v.Enabled = enabled != 0
		v.Available = available != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// VirtualTargetInput is one ordered target to persist.
type VirtualTargetInput struct {
	ProviderModelID string
	Enabled         bool
}

func (s *Scope) replaceVirtualTargets(ctx context.Context, virtualID string, targets []VirtualTargetInput) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM virtual_model_targets WHERE virtual_model_id=? AND account_id=?`, virtualID, s.accountID); err != nil {
		return err
	}
	now := now()
	for i, target := range targets {
		var exists int
		if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM provider_models WHERE id=? AND account_id=?`, target.ProviderModelID, s.accountID).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return ErrVirtualTargetNotFound
		}
		targetID, err := id.New()
		if err != nil {
			return err
		}
		if _, err := s.q.ExecContext(ctx, `INSERT INTO virtual_model_targets(id,account_id,virtual_model_id,provider_model_id,position,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, targetID, s.accountID, virtualID, target.ProviderModelID, i+1, boolInt(target.Enabled), now, now); err != nil {
			return err
		}
	}
	return nil
}

// CreateVirtualModelInput carries a validated create request. When GroupID is
// empty a new group is created from NewGroupID/GroupName.
type CreateVirtualModelInput struct {
	ID          string
	GroupID     string
	NewGroupID  string
	GroupName   string
	Name        string
	RoutingMode string
	Targets     []VirtualTargetInput
}

// CreateVirtualModel inserts a virtual model, its targets and permissions,
// optionally creating its group first.
func (s *Scope) CreateVirtualModel(ctx context.Context, in CreateVirtualModelInput) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		if tx.enforceLimits {
			if err := tx.EnforceVirtualModelLimit(ctx); err != nil {
				return err
			}
		}
		now := now()
		groupID := in.GroupID
		if groupID == "" {
			groupID = in.NewGroupID
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO namespaces(account_id,name,kind,entity_id) VALUES(?,?,'virtual',?)`, tx.accountID, in.GroupName, groupID); err != nil {
				return err
			}
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO virtual_provider_groups(id,account_id,name,created_at,updated_at) VALUES(?,?,?,?,?)`, groupID, tx.accountID, in.GroupName, now, now); err != nil {
				return err
			}
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at,account_id) SELECT id,'virtual',?,0,?,? FROM client_keys WHERE account_id=?`, groupID, now, tx.accountID, tx.accountID); err != nil {
				return err
			}
		}
		primary := in.Targets[0].ProviderModelID
		var primaryProvider string
		if err := tx.q.QueryRowContext(ctx, `SELECT provider_id FROM provider_models WHERE id=? AND account_id=?`, primary, tx.accountID).Scan(&primaryProvider); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrVirtualTargetNotFound
			}
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO virtual_models(id,account_id,virtual_group_id,name,target_provider_id,target_provider_model_id,routing_mode,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, in.ID, tx.accountID, groupID, in.Name, primaryProvider, primary, in.RoutingMode, now, now); err != nil {
			return err
		}
		if err := tx.replaceVirtualTargets(ctx, in.ID, in.Targets); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at,account_id) SELECT c.id,'virtual',?,coalesce(d.new_models_enabled,0),?,?,? FROM client_keys c LEFT JOIN client_group_defaults d ON d.client_key_id=c.id AND d.group_kind='virtual' AND d.group_id=? AND d.account_id=? WHERE c.account_id=?`, in.ID, now, now, tx.accountID, groupID, tx.accountID, tx.accountID); err != nil {
			return err
		}
		return nil
	})
}

// VirtualModelEditable is the current state read before an update.
type VirtualModelEditable struct {
	Name             string
	TargetProviderID string
	TargetModelID    string
	RoutingMode      string
}

func (s *Scope) GetVirtualModelEditable(ctx context.Context, id string) (VirtualModelEditable, error) {
	var v VirtualModelEditable
	err := s.q.QueryRowContext(ctx, `SELECT name,target_provider_id,target_provider_model_id,routing_mode FROM virtual_models WHERE id=? AND account_id=?`, id, s.accountID).
		Scan(&v.Name, &v.TargetProviderID, &v.TargetModelID, &v.RoutingMode)
	if errors.Is(err, sql.ErrNoRows) {
		return VirtualModelEditable{}, ErrVirtualModelNotFound
	}
	return v, err
}

// UpdateVirtualModelInput is the fully merged update request.
type UpdateVirtualModelInput struct {
	ID             string
	Name           string
	TargetProvider string
	TargetModel    string
	RoutingMode    string
	ReplaceTargets bool
	Targets        []VirtualTargetInput
}

// UpdateVirtualModel writes the merged virtual-model fields and optionally
// replaces its ordered target list.
func (s *Scope) UpdateVirtualModel(ctx context.Context, in UpdateVirtualModelInput) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		if in.ReplaceTargets {
			primary := in.Targets[0].ProviderModelID
			var primaryProvider string
			if err := tx.q.QueryRowContext(ctx, `SELECT provider_id FROM provider_models WHERE id=? AND account_id=?`, primary, tx.accountID).Scan(&primaryProvider); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrVirtualTargetNotFound
				}
				return err
			}
			in.TargetProvider, in.TargetModel = primaryProvider, primary
		}
		res, err := tx.q.ExecContext(ctx, `UPDATE virtual_models SET name=?,target_provider_id=?,target_provider_model_id=?,routing_mode=?,updated_at=? WHERE id=? AND account_id=?`, in.Name, in.TargetProvider, in.TargetModel, in.RoutingMode, now(), in.ID, tx.accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrVirtualModelNotFound
		}
		if in.ReplaceTargets {
			return tx.replaceVirtualTargets(ctx, in.ID, in.Targets)
		}
		return nil
	})
}

// DeleteVirtualModel removes a virtual model. It returns ErrVirtualBindingInUse
// when a Single client key points at it.
func (s *Scope) DeleteVirtualModel(ctx context.Context, modelID string) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		var bindings int
		if err := tx.q.QueryRowContext(ctx, `SELECT count(*) FROM client_single_bindings b JOIN client_keys c ON c.id = b.client_key_id WHERE b.virtual_model_id=? AND c.key_type='single' AND b.account_id=? AND c.account_id=?`, modelID, tx.accountID, tx.accountID).Scan(&bindings); err != nil {
			return err
		}
		if bindings > 0 {
			return ErrVirtualBindingInUse
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM client_single_bindings WHERE virtual_model_id=? AND client_key_id IN (SELECT id FROM client_keys WHERE key_type='catalogue' AND account_id=?) AND account_id=?`, modelID, tx.accountID, tx.accountID); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM client_model_permissions WHERE model_kind='virtual' AND model_id=? AND account_id=?`, modelID, tx.accountID); err != nil {
			return err
		}
		res, err := tx.q.ExecContext(ctx, `DELETE FROM virtual_models WHERE id=? AND account_id=?`, modelID, tx.accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrVirtualModelNotFound
		}
		return nil
	})
}
