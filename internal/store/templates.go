package store

import (
	"context"
	"database/sql"
	"errors"
)

// Catalogue templates are saved permission matrices that catalogue client
// keys can link to. Linking is materialised: a linked client's own
// client_model_permissions and client_group_defaults rows always mirror its
// template, rewritten in the same transaction as every template change, so
// routing and catalogue listing keep reading only the client rows. Seeding of
// new providers, groups and models writes template rows alongside client rows;
// because a linked client's feeder defaults equal the template's, both seed
// to the same value.

var (
	ErrTemplateNotFound       = errors.New("store: catalogue template not found")
	ErrClientLinkedToTemplate = errors.New("store: client key is linked to a catalogue template")
)

// Starting sets for a new template.
const (
	TemplateStartEmpty  = "empty"
	TemplateStartAll    = "all"
	TemplateStartClient = "client"
)

// permissionMatrix names the owner table and child tables of one permission
// matrix, so clients and templates share the list/update code.
type permissionMatrix struct {
	owners   string
	ownerCol string
	defaults string
	perms    string
	notFound error
}

var (
	clientMatrix   = permissionMatrix{owners: "client_keys", ownerCol: "client_key_id", defaults: "client_group_defaults", perms: "client_model_permissions", notFound: ErrClientKeyNotFound}
	templateMatrix = permissionMatrix{owners: "catalogue_templates", ownerCol: "template_id", defaults: "catalogue_template_group_defaults", perms: "catalogue_template_model_permissions", notFound: ErrTemplateNotFound}
)

// listMatrix returns the account's permission matrix for one owner.
func (s *Scope) listMatrix(ctx context.Context, m permissionMatrix, ownerID string) ([]PermissionGroupRow, error) {
	var exists int
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM `+m.owners+` WHERE id=? AND account_id=?`, ownerID, s.accountID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, m.notFound
	}
	groups := []PermissionGroupRow{}

	realRows, err := s.q.QueryContext(ctx, `SELECT p.id,p.name,coalesce(d.new_models_enabled,0) FROM providers p LEFT JOIN `+m.defaults+` d ON d.`+m.ownerCol+`=? AND d.group_kind='real' AND d.group_id=p.id AND d.account_id=? WHERE p.account_id=? ORDER BY p.name`, ownerID, s.accountID, s.accountID)
	if err != nil {
		return nil, err
	}
	realGroups, err := scanPermissionGroups(realRows, "real")
	if err != nil {
		return nil, err
	}
	if len(realGroups) > 0 {
		// One query for every group on the page, not one query per group.
		placeholders, args := permissionGroupArgs(ownerID, s.accountID, realGroups)
		modelRows, err := s.q.QueryContext(ctx, `SELECT m.provider_id,m.id,p.name||'/'||m.upstream_model_id,coalesce(x.enabled,0),m.available FROM provider_models m JOIN providers p ON p.id=m.provider_id LEFT JOIN `+m.perms+` x ON x.`+m.ownerCol+`=? AND x.model_kind='real' AND x.model_id=m.id AND x.account_id=? WHERE m.provider_id IN (`+placeholders+`) AND m.account_id=? AND p.account_id=? ORDER BY m.upstream_model_id`, args...)
		if err != nil {
			return nil, err
		}
		if err := scanPermissionModels(modelRows, "real", realGroups); err != nil {
			return nil, err
		}
	}
	groups = append(groups, realGroups...)

	virtualRows, err := s.q.QueryContext(ctx, `SELECT g.id,g.name,coalesce(d.new_models_enabled,0) FROM virtual_provider_groups g LEFT JOIN `+m.defaults+` d ON d.`+m.ownerCol+`=? AND d.group_kind='virtual' AND d.group_id=g.id AND d.account_id=? WHERE g.account_id=? ORDER BY g.name`, ownerID, s.accountID, s.accountID)
	if err != nil {
		return nil, err
	}
	virtualGroups, err := scanPermissionGroups(virtualRows, "virtual")
	if err != nil {
		return nil, err
	}
	if len(virtualGroups) > 0 {
		placeholders, args := permissionGroupArgs(ownerID, s.accountID, virtualGroups)
		modelRows, err := s.q.QueryContext(ctx, `SELECT v.virtual_group_id,v.id,g.name||'/'||v.name,coalesce(x.enabled,0),EXISTS(SELECT 1 FROM virtual_model_targets t JOIN provider_models m2 ON m2.id=t.provider_model_id JOIN providers p2 ON p2.id=m2.provider_id WHERE t.virtual_model_id=v.id AND t.enabled=1 AND m2.available=1 AND p2.enabled=1) FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id LEFT JOIN `+m.perms+` x ON x.`+m.ownerCol+`=? AND x.model_kind='virtual' AND x.model_id=v.id AND x.account_id=? WHERE v.virtual_group_id IN (`+placeholders+`) AND v.account_id=? AND g.account_id=? ORDER BY v.name`, args...)
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

// applyMatrixUpdates validates and upserts feeder defaults and model
// permissions for one owner. It must run inside a transaction; the caller has
// already confirmed the owner exists in the account.
func (s *Scope) applyMatrixUpdates(ctx context.Context, m permissionMatrix, ownerID string, defaults []PermissionDefaultUpdate, permissions []PermissionModelUpdate) error {
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
		if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE id=? AND account_id=?`, d.GroupID, s.accountID).Scan(&valid); err != nil {
			return err
		}
		if valid == 0 {
			return ErrPermissionGroupNotFound
		}
		if _, err := s.q.ExecContext(ctx, `INSERT INTO `+m.defaults+`(`+m.ownerCol+`,group_kind,group_id,new_models_enabled,updated_at,account_id) VALUES(?,?,?,?,?,?) ON CONFLICT(`+m.ownerCol+`,group_kind,group_id) DO UPDATE SET new_models_enabled=excluded.new_models_enabled,updated_at=excluded.updated_at`, ownerID, d.Kind, d.GroupID, boolInt(d.Enabled), now, s.accountID); err != nil {
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
		if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE id=? AND account_id=?`, p.ModelID, s.accountID).Scan(&valid); err != nil {
			return err
		}
		if valid == 0 {
			return ErrPermissionModelNotFound
		}
		if _, err := s.q.ExecContext(ctx, `INSERT INTO `+m.perms+`(`+m.ownerCol+`,model_kind,model_id,enabled,created_at,updated_at,account_id) VALUES(?,?,?,?,?,?,?) ON CONFLICT(`+m.ownerCol+`,model_kind,model_id) DO UPDATE SET enabled=excluded.enabled,updated_at=excluded.updated_at`, ownerID, p.Kind, p.ModelID, boolInt(p.Enabled), now, now, s.accountID); err != nil {
			return err
		}
	}
	return nil
}

// TemplateRow is one catalogue template with its linked-client count.
type TemplateRow struct {
	ID          string
	Name        string
	Description string
	CreatedAt   string
	UpdatedAt   string
	ClientCount int
}

const templateSelect = `SELECT t.id,t.name,t.description,t.created_at,t.updated_at,(SELECT count(*) FROM client_keys c WHERE c.template_id=t.id AND c.account_id=t.account_id) FROM catalogue_templates t`

// ListTemplates returns the account's catalogue templates ordered by name.
func (s *Scope) ListTemplates(ctx context.Context) ([]TemplateRow, error) {
	rows, err := s.q.QueryContext(ctx, templateSelect+` WHERE t.account_id=? ORDER BY t.name`, s.accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TemplateRow{}
	for rows.Next() {
		var v TemplateRow
		if err := rows.Scan(&v.ID, &v.Name, &v.Description, &v.CreatedAt, &v.UpdatedAt, &v.ClientCount); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetTemplate returns one of the account's catalogue templates.
func (s *Scope) GetTemplate(ctx context.Context, id string) (TemplateRow, error) {
	var v TemplateRow
	err := s.q.QueryRowContext(ctx, templateSelect+` WHERE t.id=? AND t.account_id=?`, id, s.accountID).Scan(&v.ID, &v.Name, &v.Description, &v.CreatedAt, &v.UpdatedAt, &v.ClientCount)
	if errors.Is(err, sql.ErrNoRows) {
		return TemplateRow{}, ErrTemplateNotFound
	}
	return v, err
}

// CreateTemplateInput describes a new template and its starting matrix.
// StartFrom is TemplateStartEmpty, TemplateStartAll or TemplateStartClient;
// SourceClientID names the client copied by TemplateStartClient.
type CreateTemplateInput struct {
	ID             string
	Name           string
	Description    string
	StartFrom      string
	SourceClientID string
}

// CreateTemplate persists a template and a complete matrix row set for every
// provider, virtual group and model in the account.
func (s *Scope) CreateTemplate(ctx context.Context, in CreateTemplateInput) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		now := now()
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO catalogue_templates(id,account_id,name,description,created_at,updated_at) VALUES(?,?,?,?,?,?)`, in.ID, tx.accountID, in.Name, in.Description, now, now); err != nil {
			return err
		}
		fill := 0
		switch in.StartFrom {
		case TemplateStartAll:
			fill = 1
		case TemplateStartClient:
			var exists int
			if err := tx.q.QueryRowContext(ctx, `SELECT count(*) FROM client_keys WHERE id=? AND account_id=?`, in.SourceClientID, tx.accountID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return ErrClientKeyNotFound
			}
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO catalogue_template_group_defaults(template_id,group_kind,group_id,new_models_enabled,updated_at,account_id) SELECT ?,group_kind,group_id,new_models_enabled,?,? FROM client_group_defaults WHERE client_key_id=? AND account_id=?`, in.ID, now, tx.accountID, in.SourceClientID, tx.accountID); err != nil {
				return err
			}
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO catalogue_template_model_permissions(template_id,model_kind,model_id,enabled,created_at,updated_at,account_id) SELECT ?,model_kind,model_id,enabled,?,?,? FROM client_model_permissions WHERE client_key_id=? AND account_id=?`, in.ID, now, now, tx.accountID, in.SourceClientID, tx.accountID); err != nil {
				return err
			}
		}
		// Fill every row the start set did not supply, so the matrix is complete.
		statements := []string{
			`INSERT INTO catalogue_template_group_defaults(template_id,group_kind,group_id,new_models_enabled,updated_at,account_id) SELECT ?,'real',id,?,?,? FROM providers WHERE account_id=? ON CONFLICT DO NOTHING`,
			`INSERT INTO catalogue_template_group_defaults(template_id,group_kind,group_id,new_models_enabled,updated_at,account_id) SELECT ?,'virtual',id,?,?,? FROM virtual_provider_groups WHERE account_id=? ON CONFLICT DO NOTHING`,
		}
		for _, stmt := range statements {
			if _, err := tx.q.ExecContext(ctx, stmt, in.ID, fill, now, tx.accountID, tx.accountID); err != nil {
				return err
			}
		}
		statements = []string{
			`INSERT INTO catalogue_template_model_permissions(template_id,model_kind,model_id,enabled,created_at,updated_at,account_id) SELECT ?,'real',id,?,?,?,? FROM provider_models WHERE account_id=? ON CONFLICT DO NOTHING`,
			`INSERT INTO catalogue_template_model_permissions(template_id,model_kind,model_id,enabled,created_at,updated_at,account_id) SELECT ?,'virtual',id,?,?,?,? FROM virtual_models WHERE account_id=? ON CONFLICT DO NOTHING`,
		}
		for _, stmt := range statements {
			if _, err := tx.q.ExecContext(ctx, stmt, in.ID, fill, now, now, tx.accountID, tx.accountID); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpdateTemplate renames or re-describes a template. Nil fields are left as
// they are.
func (s *Scope) UpdateTemplate(ctx context.Context, id string, name, description *string) error {
	set := "updated_at=?"
	args := []any{now()}
	if name != nil {
		set += ",name=?"
		args = append(args, *name)
	}
	if description != nil {
		set += ",description=?"
		args = append(args, *description)
	}
	args = append(args, id, s.accountID)
	res, err := s.q.ExecContext(ctx, `UPDATE catalogue_templates SET `+set+` WHERE id=? AND account_id=?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTemplateNotFound
	}
	return nil
}

// DeleteTemplate deletes a template. Linked clients become Custom; their rows
// already mirror the template, so their access is unchanged. It returns the
// detached client ids.
func (s *Scope) DeleteTemplate(ctx context.Context, id string) ([]string, error) {
	var detached []string
	err := s.RunTx(ctx, nil, func(tx *Scope) error {
		linked, err := tx.linkedClientIDs(ctx, id)
		if err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `UPDATE client_keys SET template_id=NULL,updated_at=? WHERE template_id=? AND account_id=?`, now(), id, tx.accountID); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM catalogue_template_model_permissions WHERE template_id=? AND account_id=?`, id, tx.accountID); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM catalogue_template_group_defaults WHERE template_id=? AND account_id=?`, id, tx.accountID); err != nil {
			return err
		}
		res, err := tx.q.ExecContext(ctx, `DELETE FROM catalogue_templates WHERE id=? AND account_id=?`, id, tx.accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrTemplateNotFound
		}
		detached = linked
		return nil
	})
	return detached, err
}

// ListTemplatePermissions returns a template's permission matrix.
func (s *Scope) ListTemplatePermissions(ctx context.Context, id string) ([]PermissionGroupRow, error) {
	return s.listMatrix(ctx, templateMatrix, id)
}

// UpdateTemplatePermissions applies matrix updates to a template and rewrites
// every linked client to match, in one transaction. It returns the linked
// client ids.
func (s *Scope) UpdateTemplatePermissions(ctx context.Context, id string, defaults []PermissionDefaultUpdate, permissions []PermissionModelUpdate) ([]string, error) {
	var linked []string
	err := s.RunTx(ctx, nil, func(tx *Scope) error {
		var exists int
		if err := tx.q.QueryRowContext(ctx, `SELECT count(*) FROM catalogue_templates WHERE id=? AND account_id=?`, id, tx.accountID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrTemplateNotFound
		}
		if err := tx.applyMatrixUpdates(ctx, templateMatrix, id, defaults, permissions); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `UPDATE catalogue_templates SET updated_at=? WHERE id=? AND account_id=?`, now(), id, tx.accountID); err != nil {
			return err
		}
		ids, err := tx.linkedClientIDs(ctx, id)
		if err != nil {
			return err
		}
		linked = ids
		return tx.syncTemplateClients(ctx, id, "")
	})
	return linked, err
}

func (s *Scope) linkedClientIDs(ctx context.Context, templateID string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id FROM client_keys WHERE template_id=? AND account_id=? ORDER BY id`, templateID, s.accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// templateExists reports whether a template belongs to the scope's account.
func (s *Scope) templateExists(ctx context.Context, id string) (bool, error) {
	var exists int
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM catalogue_templates WHERE id=? AND account_id=?`, id, s.accountID).Scan(&exists); err != nil {
		return false, err
	}
	return exists == 1, nil
}

// syncTemplateClients replaces the matrix rows of the template's linked
// clients (or only clientID when it is non-empty) with the template's rows.
// It must run inside a transaction.
func (s *Scope) syncTemplateClients(ctx context.Context, templateID, clientID string) error {
	now := now()
	filter := ""
	args := []any{templateID, s.accountID}
	if clientID != "" {
		filter = ` AND id=?`
		args = append(args, clientID)
	}
	linked := `SELECT id FROM client_keys WHERE template_id=? AND account_id=?` + filter
	if _, err := s.q.ExecContext(ctx, `DELETE FROM client_group_defaults WHERE account_id=? AND client_key_id IN (`+linked+`)`, append([]any{s.accountID}, args...)...); err != nil {
		return err
	}
	if _, err := s.q.ExecContext(ctx, `DELETE FROM client_model_permissions WHERE account_id=? AND client_key_id IN (`+linked+`)`, append([]any{s.accountID}, args...)...); err != nil {
		return err
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at,account_id) SELECT c.id,d.group_kind,d.group_id,d.new_models_enabled,?,? FROM catalogue_template_group_defaults d JOIN (`+linked+`) c WHERE d.template_id=? AND d.account_id=?`, append(append([]any{now, s.accountID}, args...), templateID, s.accountID)...); err != nil {
		return err
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at,account_id) SELECT c.id,x.model_kind,x.model_id,x.enabled,?,?,? FROM catalogue_template_model_permissions x JOIN (`+linked+`) c WHERE x.template_id=? AND x.account_id=?`, append(append([]any{now, now, s.accountID}, args...), templateID, s.accountID)...); err != nil {
		return err
	}
	return nil
}

// linkClientTemplate points a catalogue client at a template (overwriting its
// matrix with the template's) or, with an empty templateID, detaches it to
// Custom (its rows already mirror the template, so access is unchanged). It
// must run inside a transaction.
func (s *Scope) linkClientTemplate(ctx context.Context, clientID, templateID string) error {
	if templateID == "" {
		_, err := s.q.ExecContext(ctx, `UPDATE client_keys SET template_id=NULL WHERE id=? AND account_id=?`, clientID, s.accountID)
		return err
	}
	ok, err := s.templateExists(ctx, templateID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrTemplateNotFound
	}
	if _, err := s.q.ExecContext(ctx, `UPDATE client_keys SET template_id=? WHERE id=? AND account_id=?`, templateID, clientID, s.accountID); err != nil {
		return err
	}
	return s.syncTemplateClients(ctx, templateID, clientID)
}
