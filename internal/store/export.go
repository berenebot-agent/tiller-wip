package store

import "context"

// This file owns the account-scoped queries backing the customer data export
// (docs/stage_d_api_contract.md GET /api/auth/account/export) and the
// onboarding "has this account routed a successful request yet?" check. Every
// query binds the scope's account_id; none of them surface a credential, a
// credential hash, or an API-key selector.

// AccountExportRowCap bounds how many Activity plus audit rows one export may
// stream. It is a safety valve against an unbounded ZIP rather than a product
// limit.
const AccountExportRowCap = 100000

// AccountExportProvider is the credential-free provider record in config.json.
type AccountExportProvider struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	BaseURL   string `json:"base_url"`
	Enabled   bool   `json:"enabled"`
	Protocols string `json:"protocols"`
}

// AccountExportModel is one provider catalogue entry in config.json.
type AccountExportModel struct {
	ID               string `json:"id"`
	ProviderID       string `json:"provider_id"`
	UpstreamModelID  string `json:"upstream_model_id"`
	CanonicalModelID string `json:"canonical_model_id"`
	DisplayName      string `json:"display_name"`
	ContextLength    *int64 `json:"context_length,omitempty"`
	MaxOutputTokens  *int64 `json:"max_output_tokens,omitempty"`
	NativeProtocol   string `json:"native_protocol,omitempty"`
	Available        bool   `json:"available"`
	Origin           string `json:"origin"`
}

// AccountExportVirtualGroup is one virtual provider group in config.json.
type AccountExportVirtualGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AccountExportVirtualModel is one virtual model in config.json.
type AccountExportVirtualModel struct {
	ID               string `json:"id"`
	VirtualGroupID   string `json:"virtual_group_id"`
	Name             string `json:"name"`
	RoutingMode      string `json:"routing_mode"`
	TargetProviderID string `json:"target_provider_id,omitempty"`
	TargetModelID    string `json:"target_provider_model_id,omitempty"`
}

// AccountExportVirtualTarget is one ordered virtual-model target in config.json.
type AccountExportVirtualTarget struct {
	ID              string `json:"id"`
	VirtualModelID  string `json:"virtual_model_id"`
	ProviderModelID string `json:"provider_model_id"`
	Position        int    `json:"position"`
	Enabled         bool   `json:"enabled"`
}

// AccountExportClientKey is the non-secret client-key metadata in config.json.
// Secret hashes, selectors and fingerprints are deliberately absent.
type AccountExportClientKey struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Group         string `json:"key_group"`
	Type          string `json:"type"`
	Enabled       bool   `json:"enabled"`
	RetentionDays int    `json:"retention_days"`
	CreatedAt     string `json:"created_at"`
	RotatedAt     string `json:"rotated_at,omitempty"`
}

// AccountExportConfig is the tenant-owned portion of config.json.
type AccountExportConfig struct {
	Settings       map[string]string            `json:"settings"`
	Providers      []AccountExportProvider      `json:"providers"`
	Models         []AccountExportModel         `json:"provider_models"`
	VirtualGroups  []AccountExportVirtualGroup  `json:"virtual_groups"`
	VirtualModels  []AccountExportVirtualModel  `json:"virtual_models"`
	VirtualTargets []AccountExportVirtualTarget `json:"virtual_targets"`
	ClientKeys     []AccountExportClientKey     `json:"client_keys"`
}

// BuildAccountExportConfig reads the account's providers, catalogue, virtual
// routing, client-key metadata and non-secret settings in a series of short,
// account-scoped reads (no long-lived transaction).
func (s *Scope) BuildAccountExportConfig(ctx context.Context) (AccountExportConfig, error) {
	cfg := AccountExportConfig{
		Settings:       map[string]string{},
		Providers:      []AccountExportProvider{},
		Models:         []AccountExportModel{},
		VirtualGroups:  []AccountExportVirtualGroup{},
		VirtualModels:  []AccountExportVirtualModel{},
		VirtualTargets: []AccountExportVirtualTarget{},
		ClientKeys:     []AccountExportClientKey{},
	}

	settingRows, err := s.q.QueryContext(ctx, `SELECT key,value FROM settings WHERE account_id=?`, s.accountID)
	if err != nil {
		return cfg, err
	}
	for settingRows.Next() {
		var key, value string
		if err := settingRows.Scan(&key, &value); err != nil {
			settingRows.Close()
			return cfg, err
		}
		if secretSettingKey(key) {
			continue
		}
		cfg.Settings[key] = value
	}
	if err := settingRows.Close(); err != nil {
		return cfg, err
	}
	if err := settingRows.Err(); err != nil {
		return cfg, err
	}

	providerRows, err := s.q.QueryContext(ctx, `SELECT id,name,type,base_url,enabled,protocols FROM providers WHERE account_id=? ORDER BY name`, s.accountID)
	if err != nil {
		return cfg, err
	}
	for providerRows.Next() {
		var v AccountExportProvider
		var enabled int
		if err := providerRows.Scan(&v.ID, &v.Name, &v.Type, &v.BaseURL, &enabled, &v.Protocols); err != nil {
			providerRows.Close()
			return cfg, err
		}
		v.Enabled = enabled != 0
		cfg.Providers = append(cfg.Providers, v)
	}
	if err := providerRows.Close(); err != nil {
		return cfg, err
	}
	if err := providerRows.Err(); err != nil {
		return cfg, err
	}

	modelRows, err := s.q.QueryContext(ctx, `SELECT m.id,m.provider_id,m.upstream_model_id,p.name||'/'||m.upstream_model_id,m.display_name,m.context_length,m.max_output_tokens,coalesce(m.native_protocol,''),m.available,m.origin FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE m.account_id=? AND p.account_id=? ORDER BY p.name,m.upstream_model_id`, s.accountID, s.accountID)
	if err != nil {
		return cfg, err
	}
	for modelRows.Next() {
		var v AccountExportModel
		var available int
		if err := modelRows.Scan(&v.ID, &v.ProviderID, &v.UpstreamModelID, &v.CanonicalModelID, &v.DisplayName, &v.ContextLength, &v.MaxOutputTokens, &v.NativeProtocol, &available, &v.Origin); err != nil {
			modelRows.Close()
			return cfg, err
		}
		v.Available = available != 0
		cfg.Models = append(cfg.Models, v)
	}
	if err := modelRows.Close(); err != nil {
		return cfg, err
	}
	if err := modelRows.Err(); err != nil {
		return cfg, err
	}

	groupRows, err := s.q.QueryContext(ctx, `SELECT id,name FROM virtual_provider_groups WHERE account_id=? ORDER BY name`, s.accountID)
	if err != nil {
		return cfg, err
	}
	for groupRows.Next() {
		var v AccountExportVirtualGroup
		if err := groupRows.Scan(&v.ID, &v.Name); err != nil {
			groupRows.Close()
			return cfg, err
		}
		cfg.VirtualGroups = append(cfg.VirtualGroups, v)
	}
	if err := groupRows.Close(); err != nil {
		return cfg, err
	}
	if err := groupRows.Err(); err != nil {
		return cfg, err
	}

	virtualRows, err := s.q.QueryContext(ctx, `SELECT id,virtual_group_id,name,routing_mode,coalesce(target_provider_id,''),coalesce(target_provider_model_id,'') FROM virtual_models WHERE account_id=? ORDER BY name`, s.accountID)
	if err != nil {
		return cfg, err
	}
	for virtualRows.Next() {
		var v AccountExportVirtualModel
		if err := virtualRows.Scan(&v.ID, &v.VirtualGroupID, &v.Name, &v.RoutingMode, &v.TargetProviderID, &v.TargetModelID); err != nil {
			virtualRows.Close()
			return cfg, err
		}
		cfg.VirtualModels = append(cfg.VirtualModels, v)
	}
	if err := virtualRows.Close(); err != nil {
		return cfg, err
	}
	if err := virtualRows.Err(); err != nil {
		return cfg, err
	}

	targetRows, err := s.q.QueryContext(ctx, `SELECT id,virtual_model_id,provider_model_id,position,enabled FROM virtual_model_targets WHERE account_id=? ORDER BY virtual_model_id,position`, s.accountID)
	if err != nil {
		return cfg, err
	}
	for targetRows.Next() {
		var v AccountExportVirtualTarget
		var enabled int
		if err := targetRows.Scan(&v.ID, &v.VirtualModelID, &v.ProviderModelID, &v.Position, &enabled); err != nil {
			targetRows.Close()
			return cfg, err
		}
		v.Enabled = enabled != 0
		cfg.VirtualTargets = append(cfg.VirtualTargets, v)
	}
	if err := targetRows.Close(); err != nil {
		return cfg, err
	}
	if err := targetRows.Err(); err != nil {
		return cfg, err
	}

	clientRows, err := s.q.QueryContext(ctx, `SELECT id,name,description,key_group,key_type,enabled,retention_days,created_at,coalesce(rotated_at,'') FROM client_keys WHERE account_id=? ORDER BY name`, s.accountID)
	if err != nil {
		return cfg, err
	}
	for clientRows.Next() {
		var v AccountExportClientKey
		var enabled int
		if err := clientRows.Scan(&v.ID, &v.Name, &v.Description, &v.Group, &v.Type, &enabled, &v.RetentionDays, &v.CreatedAt, &v.RotatedAt); err != nil {
			clientRows.Close()
			return cfg, err
		}
		v.Enabled = enabled != 0
		cfg.ClientKeys = append(cfg.ClientKeys, v)
	}
	if err := clientRows.Close(); err != nil {
		return cfg, err
	}
	return cfg, clientRows.Err()
}

// FirstRoutedRequestAt returns the created_at timestamp of the account's
// earliest 2xx routed request, or "" when the account has none. It powers the
// onboarding wizard state and returns ErrActivityUnavailable when Activity is
// not open.
func (s *Scope) FirstRoutedRequestAt(ctx context.Context) (string, error) {
	var first *string
	err := s.withActivity(ctx, func(q querier) error {
		return q.QueryRowContext(ctx, `SELECT min(created_at) FROM request_logs WHERE account_id=? AND http_status>=200 AND http_status<300`, s.accountID).Scan(&first)
	})
	if err != nil {
		return "", err
	}
	if first == nil {
		return "", nil
	}
	return *first, nil
}

// CountAccountActivityRows counts the account's request_logs rows. The export
// checks it before emitting any bytes so an oversized export is rejected with a
// clean 413 rather than a truncated response.
func (s *Scope) CountAccountActivityRows(ctx context.Context) (int, error) {
	var count int
	err := s.withActivity(ctx, func(q querier) error {
		return q.QueryRowContext(ctx, `SELECT count(*) FROM request_logs WHERE account_id=?`, s.accountID).Scan(&count)
	})
	return count, err
}

// CountAccountAuditRows counts the account's audit events for the export cap.
func (s *Scope) CountAccountAuditRows(ctx context.Context) (int, error) {
	var count int
	err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM account_audit_events WHERE account_id=?`, s.accountID).Scan(&count)
	return count, err
}

// ExportAccountActivity streams every request_logs row for the account, newest
// first, in bounded pages. It reuses the same paging logic as the client and
// virtual Activity exports, so no tenant transaction spans the stream.
func (s *Scope) ExportAccountActivity(ctx context.Context, fn func(ActivityRow) error) error {
	return s.exportActivity(ctx, "1=1", nil, "", fn)
}
