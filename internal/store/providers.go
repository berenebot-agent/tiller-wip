package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	ErrProviderNotFound   = errors.New("store: provider not found")
	ErrSingleBindingInUse = errors.New("store: provider referenced by a single client key")
)

// ProviderInUseError reports a provider that is the last target in one or more
// virtual model chains, so it cannot be deleted. Blocked lists the terminal
// chains; Referenced lists every chain that references the provider.
type ProviderInUseError struct {
	Blocked    []string
	Referenced []string
}

func (e *ProviderInUseError) Error() string {
	return fmt.Sprintf("provider is the last target in %d virtual model chain(s)", len(e.Blocked))
}

// ProviderRow is one row of the admin provider list.
type ProviderRow struct {
	ID                   string
	Name                 string
	Type                 string
	BaseURL              string
	Enabled              bool
	Protocols            string
	CredentialConfigured bool
	AuthState            string
	LastRefreshAt        *string
	NextRefreshAt        *string
	LastRefreshError     *string
	CreatedAt            string
	UpdatedAt            string
	ModelCount           int
	AvailableModelCount  int
}

// ProviderFilter narrows ListProviders.
type ProviderFilter struct {
	Search string
	Limit  int
	Offset int
}

func (s *Scope) ListProviders(ctx context.Context, filter ProviderFilter) ([]ProviderRow, error) {
	pattern := "%" + filter.Search + "%"
	rows, err := s.q.QueryContext(ctx, `SELECT p.id,p.name,p.type,p.base_url,p.enabled,p.protocols,(p.credential_secret IS NOT NULL OR EXISTS(SELECT 1 FROM provider_oauth_tokens o WHERE o.provider_id=p.id)),coalesce(o.auth_state,''),p.last_refresh_at,p.next_refresh_at,p.last_refresh_error,p.created_at,p.updated_at,count(m.id),coalesce(sum(CASE WHEN m.available=1 THEN 1 ELSE 0 END),0) FROM providers p LEFT JOIN provider_models m ON m.provider_id=p.id LEFT JOIN provider_oauth_tokens o ON o.provider_id=p.id WHERE (p.name LIKE ? OR p.type LIKE ?) AND p.account_id=? GROUP BY p.id ORDER BY p.name LIMIT ? OFFSET ?`, pattern, pattern, s.accountID, filter.Limit, filter.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderRow{}
	for rows.Next() {
		var v ProviderRow
		var enabled, configured int
		if err := rows.Scan(&v.ID, &v.Name, &v.Type, &v.BaseURL, &enabled, &v.Protocols, &configured, &v.AuthState, &v.LastRefreshAt, &v.NextRefreshAt, &v.LastRefreshError, &v.CreatedAt, &v.UpdatedAt, &v.ModelCount, &v.AvailableModelCount); err != nil {
			return nil, err
		}
		v.Enabled = enabled != 0
		v.CredentialConfigured = configured != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// CreateProviderInput carries a validated provider create request.
type CreateProviderInput struct {
	ID         string
	Name       string
	Type       string
	BaseURL    string
	Credential string
	Enabled    bool
	Protocols  string
}

// CreateProvider inserts the provider namespace and row, then seeds a real
// group default for every client key in the same account.
func (s *Scope) CreateProvider(ctx context.Context, in CreateProviderInput) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		now := now()
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO namespaces(account_id,name,kind,entity_id) VALUES(?,?,'real',?)`, tx.accountID, in.Name, in.ID); err != nil {
			return err
		}
		credential, err := tx.encryptSecret(secretAAD(tx.accountID, "provider", in.ID, "credential_secret"), in.Credential)
		if err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO providers(id,account_id,name,type,base_url,credential_secret,enabled,protocols,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, in.ID, tx.accountID, in.Name, in.Type, in.BaseURL, nullableStoreString(credential), boolInt(in.Enabled), in.Protocols, now, now); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_group_defaults(client_key_id,group_kind,group_id,new_models_enabled,updated_at,account_id) SELECT id,'real',?,0,?,? FROM client_keys WHERE account_id=?`, in.ID, now, tx.accountID, tx.accountID); err != nil {
			return err
		}
		return nil
	})
}

// ProviderEditable is the mutable provider field set read before an update.
type ProviderEditable struct {
	Name      string
	Type      string
	BaseURL   string
	Enabled   bool
	Protocols string
}

func (s *Scope) GetProviderEditable(ctx context.Context, id string) (ProviderEditable, error) {
	var v ProviderEditable
	var enabled int
	err := s.q.QueryRowContext(ctx, `SELECT name,type,base_url,enabled,protocols FROM providers WHERE id=? AND account_id=?`, id, s.accountID).
		Scan(&v.Name, &v.Type, &v.BaseURL, &enabled, &v.Protocols)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderEditable{}, ErrProviderNotFound
	}
	if err != nil {
		return ProviderEditable{}, err
	}
	v.Enabled = enabled != 0
	return v, nil
}

// UpdateProviderInput is the fully merged provider field set.
type UpdateProviderInput struct {
	ID        string
	Name      string
	BaseURL   string
	Enabled   bool
	Protocols string
}

func (s *Scope) UpdateProvider(ctx context.Context, in UpdateProviderInput) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		if _, err := tx.q.ExecContext(ctx, `UPDATE namespaces SET name=? WHERE entity_id=? AND kind='real' AND account_id=?`, in.Name, in.ID, tx.accountID); err != nil {
			return err
		}
		res, err := tx.q.ExecContext(ctx, `UPDATE providers SET name=?,base_url=?,enabled=?,protocols=?,updated_at=? WHERE id=? AND account_id=?`, in.Name, in.BaseURL, boolInt(in.Enabled), in.Protocols, now(), in.ID, tx.accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrProviderNotFound
		}
		return nil
	})
}

func (s *Scope) ProviderType(ctx context.Context, id string) (string, error) {
	var providerType string
	err := s.q.QueryRowContext(ctx, `SELECT type FROM providers WHERE id=? AND account_id=?`, id, s.accountID).Scan(&providerType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrProviderNotFound
	}
	return providerType, err
}

// ReplaceProviderCredential sets a provider's API credential. found is false
// when no provider in the account matched.
func (s *Scope) ReplaceProviderCredential(ctx context.Context, id, credential string) (bool, error) {
	stored, err := s.encryptSecret(secretAAD(s.accountID, "provider", id, "credential_secret"), credential)
	if err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `UPDATE providers SET credential_secret=?,updated_at=? WHERE id=? AND account_id=?`, stored, now(), id, s.accountID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetProviderRefreshError records a discovery failure on a provider.
func (s *Scope) SetProviderRefreshError(ctx context.Context, id, message string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE providers SET last_refresh_error=?,updated_at=? WHERE id=? AND account_id=?`, message, now(), id, s.accountID)
	return err
}

// ProviderLoad is the credential-bearing provider row used to run discovery.
type ProviderLoad struct {
	ID         string
	Name       string
	Type       string
	BaseURL    string
	Credential string
	Enabled    bool
	Protocols  string
}

func (s *Scope) LoadProvider(ctx context.Context, id string) (ProviderLoad, error) {
	var v ProviderLoad
	var enabled int
	err := s.q.QueryRowContext(ctx, `SELECT id,name,type,base_url,coalesce(credential_secret,''),enabled,protocols FROM providers WHERE id=? AND account_id=?`, id, s.accountID).
		Scan(&v.ID, &v.Name, &v.Type, &v.BaseURL, &v.Credential, &enabled, &v.Protocols)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderLoad{}, ErrProviderNotFound
	}
	if err != nil {
		return ProviderLoad{}, err
	}
	v.Credential, err = s.decryptSecret(secretAAD(s.accountID, "provider", v.ID, "credential_secret"), v.Credential)
	if err != nil {
		return ProviderLoad{}, err
	}
	v.Enabled = enabled != 0
	return v, nil
}

// ProviderRef identifies a provider due for a scheduled refresh.
type ProviderRef struct {
	AccountID  string
	ProviderID string
}

// DueProviders lists enabled providers whose next refresh is due, across every
// account. Platform-level scheduling: each returned job runs scoped to its
// account.
func (s *Store) DueProviders(ctx context.Context, dueBefore string) ([]ProviderRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id,id FROM providers WHERE enabled=1 AND (next_refresh_at IS NULL OR next_refresh_at<=?)`, dueBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderRef
	for rows.Next() {
		var ref ProviderRef
		if err := rows.Scan(&ref.AccountID, &ref.ProviderID); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// ProviderVirtualModelRef is a virtual model that references a provider, along
// with whether that provider is the last eligible target in the chain.
type ProviderVirtualModelRef struct {
	ID        string
	Canonical string
	Terminal  bool
}

func (s *Scope) providerVirtualModelRefs(ctx context.Context, providerID string) ([]ProviderVirtualModelRef, []ProviderVirtualModelRef, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT v.id, g.name||'/'||v.name, (SELECT count(*) FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=v.id AND t.enabled=1 AND m.available=1 AND p.enabled=1 AND m.provider_id<>?) AS takeover FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE v.account_id=? AND EXISTS (SELECT 1 FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id WHERE t.virtual_model_id=v.id AND m.provider_id=? AND m.account_id=?)`, providerID, s.accountID, providerID, s.accountID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var terminal, all []ProviderVirtualModelRef
	for rows.Next() {
		var v ProviderVirtualModelRef
		var takeover int
		if err := rows.Scan(&v.ID, &v.Canonical, &takeover); err != nil {
			return nil, nil, err
		}
		v.Terminal = takeover == 0
		all = append(all, v)
		if v.Terminal {
			terminal = append(terminal, v)
		}
	}
	return terminal, all, rows.Err()
}

// DeleteProvider deletes a provider and its catalogue, permissions, bindings
// and namespace within the account. It returns ErrSingleBindingInUse when a
// Single client key points at the provider, ErrProviderNotFound when the
// provider does not exist, or *ProviderInUseError when the provider is the last
// target in a virtual model chain.
func (s *Scope) DeleteProvider(ctx context.Context, providerID string) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		var singleRefs int
		if err := tx.q.QueryRowContext(ctx, `SELECT count(*) FROM client_single_bindings b JOIN client_keys c ON c.id = b.client_key_id JOIN provider_models m ON m.id = b.real_model_id WHERE m.provider_id=? AND c.key_type='single' AND b.account_id=? AND c.account_id=? AND m.account_id=?`, providerID, tx.accountID, tx.accountID, tx.accountID).Scan(&singleRefs); err != nil {
			return err
		}
		if singleRefs > 0 {
			return ErrSingleBindingInUse
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM client_single_bindings WHERE real_model_id IN (SELECT id FROM provider_models WHERE provider_id=? AND account_id=?) AND client_key_id IN (SELECT id FROM client_keys WHERE key_type='catalogue' AND account_id=?)`, providerID, tx.accountID, tx.accountID); err != nil {
			return err
		}
		terminal, all, err := tx.providerVirtualModelRefs(ctx, providerID)
		if err != nil {
			return err
		}
		if len(terminal) > 0 {
			blocked := make([]string, 0, len(terminal))
			for _, v := range terminal {
				blocked = append(blocked, v.Canonical)
			}
			referenced := make([]string, 0, len(all))
			for _, v := range all {
				referenced = append(referenced, v.Canonical)
			}
			return &ProviderInUseError{Blocked: blocked, Referenced: referenced}
		}
		for _, v := range all {
			if _, err := tx.q.ExecContext(ctx, `DELETE FROM virtual_model_targets WHERE virtual_model_id=? AND provider_model_id IN (SELECT id FROM provider_models WHERE provider_id=? AND account_id=?) AND account_id=?`, v.ID, providerID, tx.accountID, tx.accountID); err != nil {
				return err
			}
			var promotedProvider, promotedModel sql.NullString
			err := tx.q.QueryRowContext(ctx, `SELECT p.id,m.id FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=? AND t.enabled=1 AND m.available=1 AND p.enabled=1 AND t.account_id=? AND m.account_id=? AND p.account_id=? ORDER BY t.position LIMIT 1`, v.ID, tx.accountID, tx.accountID, tx.accountID).Scan(&promotedProvider, &promotedModel)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.q.ExecContext(ctx, `UPDATE virtual_models SET target_provider_id=?,target_provider_model_id=?,updated_at=? WHERE id=? AND account_id=?`, promotedProvider.String, promotedModel.String, now(), v.ID, tx.accountID); err != nil {
				return err
			}
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM client_model_permissions WHERE model_kind='real' AND model_id IN (SELECT id FROM provider_models WHERE provider_id=? AND account_id=?) AND account_id=?`, providerID, tx.accountID, tx.accountID); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM client_group_defaults WHERE group_kind='real' AND group_id=? AND account_id=?`, providerID, tx.accountID); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM provider_models WHERE provider_id=? AND account_id=?`, providerID, tx.accountID); err != nil {
			return err
		}
		res, err := tx.q.ExecContext(ctx, `DELETE FROM providers WHERE id=? AND account_id=?`, providerID, tx.accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrProviderNotFound
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM namespaces WHERE entity_id=? AND kind='real' AND account_id=?`, providerID, tx.accountID); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM provider_oauth_tokens WHERE provider_id=? AND account_id=?`, providerID, tx.accountID); err != nil {
			return err
		}
		return nil
	})
}

// DeleteAccountResources removes all tenant control-plane rows for an account
// in dependency order. Activity is deleted separately because it lives in a
// per-account database file.
func (s *Scope) DeleteAccountResources(ctx context.Context) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		statements := []string{
			`DELETE FROM client_single_bindings WHERE account_id=?`,
			`DELETE FROM client_model_permissions WHERE account_id=?`,
			`DELETE FROM client_group_defaults WHERE account_id=?`,
			`DELETE FROM client_keys WHERE account_id=?`,
			`DELETE FROM provider_oauth_tokens WHERE account_id=?`,
			`DELETE FROM virtual_model_targets WHERE account_id=?`,
			`DELETE FROM virtual_models WHERE account_id=?`,
			`DELETE FROM virtual_provider_groups WHERE account_id=?`,
			`DELETE FROM provider_models WHERE account_id=?`,
			`DELETE FROM providers WHERE account_id=?`,
			`DELETE FROM namespaces WHERE account_id=?`,
			`DELETE FROM settings WHERE account_id=?`,
		}
		for _, statement := range statements {
			if _, err := tx.q.ExecContext(ctx, statement, tx.accountID); err != nil {
				return err
			}
		}
		return nil
	})
}

// AdminHealthCounts summarizes catalogue health for the account.
type AdminHealthCounts struct {
	Providers           int
	AvailableModels     int
	RetiredModels       int
	BrokenVirtualModels int
}

func (s *Scope) AdminHealth(ctx context.Context) (AdminHealthCounts, error) {
	var c AdminHealthCounts
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM providers WHERE account_id=?`, s.accountID).Scan(&c.Providers); err != nil {
		return c, err
	}
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM provider_models WHERE available=1 AND account_id=?`, s.accountID).Scan(&c.AvailableModels); err != nil {
		return c, err
	}
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM provider_models WHERE available=0 AND account_id=?`, s.accountID).Scan(&c.RetiredModels); err != nil {
		return c, err
	}
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM virtual_models v WHERE v.account_id=? AND NOT EXISTS (SELECT 1 FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=v.id AND t.enabled=1 AND m.available=1 AND p.enabled=1 AND t.account_id=? AND m.account_id=? AND p.account_id=?)`, s.accountID, s.accountID, s.accountID, s.accountID).Scan(&c.BrokenVirtualModels); err != nil {
		return c, err
	}
	return c, nil
}
