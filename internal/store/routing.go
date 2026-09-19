package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ProviderModelCaps is the capability column set read for the model catalogue.
type ProviderModelCaps struct {
	ContextLength            sql.NullInt64
	MaxOutputTokens          sql.NullInt64
	SupportsTools            sql.NullInt64
	SupportsVision           sql.NullInt64
	SupportsReasoning        sql.NullInt64
	SupportsStructuredOutput sql.NullInt64
	ReasoningCapabilities    sql.NullString
}

func (s *Scope) ProviderModelCapsByID(ctx context.Context, id string) (ProviderModelCaps, error) {
	var v ProviderModelCaps
	err := s.q.QueryRowContext(ctx, `SELECT context_length,max_output_tokens,supports_tools,supports_vision,supports_reasoning,supports_structured_output,reasoning_capabilities FROM provider_models WHERE id=? AND account_id=?`, id, s.accountID).
		Scan(&v.ContextLength, &v.MaxOutputTokens, &v.SupportsTools, &v.SupportsVision, &v.SupportsReasoning, &v.SupportsStructuredOutput, &v.ReasoningCapabilities)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderModelCaps{}, sql.ErrNoRows
	}
	return v, err
}

// CatalogueEntry is one client-visible model in the key's catalogue.
type CatalogueEntry struct {
	Canonical                string
	ContextLength            sql.NullInt64
	MaxOutputTokens          sql.NullInt64
	SupportsTools            sql.NullInt64
	SupportsVision           sql.NullInt64
	SupportsReasoning        sql.NullInt64
	SupportsStructuredOutput sql.NullInt64
	ReasoningCapabilities    sql.NullString
	VirtualModelID           sql.NullString
}

func (s *Scope) ListCatalogueEntries(ctx context.Context, clientID string) ([]CatalogueEntry, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT canonical, context_length, max_output_tokens, supports_tools, supports_vision, supports_reasoning, supports_structured_output, reasoning_capabilities, virtual_model_id FROM (
SELECT p.name||'/'||m.upstream_model_id canonical, m.context_length, m.max_output_tokens, m.supports_tools, m.supports_vision, m.supports_reasoning, m.supports_structured_output, m.reasoning_capabilities, NULL virtual_model_id FROM client_model_permissions x JOIN provider_models m ON x.model_kind='real' AND x.model_id=m.id JOIN providers p ON p.id=m.provider_id WHERE x.client_key_id=? AND x.account_id=? AND m.account_id=? AND p.account_id=? AND x.enabled=1 AND m.available=1 AND p.enabled=1
UNION ALL
SELECT g.name||'/'||v.name canonical, NULL, NULL, NULL, NULL, NULL, NULL, NULL, v.id virtual_model_id FROM client_model_permissions x JOIN virtual_models v ON x.model_kind='virtual' AND x.model_id=v.id JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE x.client_key_id=? AND x.account_id=? AND v.account_id=? AND g.account_id=? AND x.enabled=1 AND EXISTS(SELECT 1 FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=v.id AND t.enabled=1 AND m.available=1 AND p.enabled=1 AND t.account_id=? AND m.account_id=? AND p.account_id=?)
) ORDER BY canonical`, clientID, s.accountID, s.accountID, s.accountID, clientID, s.accountID, s.accountID, s.accountID, s.accountID, s.accountID, s.accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogueEntry{}
	for rows.Next() {
		var v CatalogueEntry
		if err := rows.Scan(&v.Canonical, &v.ContextLength, &v.MaxOutputTokens, &v.SupportsTools, &v.SupportsVision, &v.SupportsReasoning, &v.SupportsStructuredOutput, &v.ReasoningCapabilities, &v.VirtualModelID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// TargetCapability is one enabled virtual target's capability columns.
type TargetCapability struct {
	VirtualModelID           string
	ContextLength            sql.NullInt64
	MaxOutputTokens          sql.NullInt64
	SupportsTools            sql.NullInt64
	SupportsVision           sql.NullInt64
	SupportsReasoning        sql.NullInt64
	SupportsStructuredOutput sql.NullInt64
	ReasoningCapabilities    sql.NullString
}

func (s *Scope) VirtualTargetCapabilities(ctx context.Context, virtualIDs []string) ([]TargetCapability, error) {
	if len(virtualIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(virtualIDs)), ",")
	args := make([]any, 0, len(virtualIDs)+3)
	for _, id := range virtualIDs {
		args = append(args, id)
	}
	args = append(args, s.accountID, s.accountID, s.accountID)
	rows, err := s.q.QueryContext(ctx, `SELECT t.virtual_model_id,m.context_length,m.max_output_tokens,m.supports_tools,m.supports_vision,m.supports_reasoning,m.supports_structured_output,m.reasoning_capabilities FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id IN (`+placeholders+`) AND t.account_id=? AND m.account_id=? AND p.account_id=? AND t.enabled=1 AND m.available=1 AND p.enabled=1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TargetCapability{}
	for rows.Next() {
		var v TargetCapability
		if err := rows.Scan(&v.VirtualModelID, &v.ContextLength, &v.MaxOutputTokens, &v.SupportsTools, &v.SupportsVision, &v.SupportsReasoning, &v.SupportsStructuredOutput, &v.ReasoningCapabilities); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RouteModelInfo is the resolved model identity for an inference request.
type RouteModelInfo struct {
	ModelID               string
	Canonical             string
	ReasoningCapabilities sql.NullString
	MaxOutputTokens       sql.NullInt64
}

// RealModelForID resolves a real model by its provider-model id.
func (s *Scope) RealModelForID(ctx context.Context, modelID string) (RouteModelInfo, error) {
	var v RouteModelInfo
	v.ModelID = modelID
	err := s.q.QueryRowContext(ctx, `SELECT p.name||'/'||m.upstream_model_id, m.reasoning_capabilities, m.max_output_tokens FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE m.id=? AND m.account_id=? AND p.account_id=?`, modelID, s.accountID, s.accountID).
		Scan(&v.Canonical, &v.ReasoningCapabilities, &v.MaxOutputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return RouteModelInfo{}, sql.ErrNoRows
	}
	return v, err
}

// PermittedRealModel resolves a catalogue key's requested real model, scoped to
// the key's enabled permissions.
func (s *Scope) PermittedRealModel(ctx context.Context, clientID, requested string) (RouteModelInfo, error) {
	var v RouteModelInfo
	err := s.q.QueryRowContext(ctx, `SELECT m.id,p.name||'/'||m.upstream_model_id, m.reasoning_capabilities, m.max_output_tokens FROM client_model_permissions x JOIN provider_models m ON x.model_kind='real' AND x.model_id=m.id JOIN providers p ON p.id=m.provider_id WHERE x.client_key_id=? AND x.account_id=? AND x.enabled=1 AND p.name||'/'||m.upstream_model_id=?`, clientID, s.accountID, requested).
		Scan(&v.ModelID, &v.Canonical, &v.ReasoningCapabilities, &v.MaxOutputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return RouteModelInfo{}, sql.ErrNoRows
	}
	return v, err
}

// PermittedVirtualModel resolves a catalogue key's requested virtual model.
func (s *Scope) PermittedVirtualModel(ctx context.Context, clientID, requested string) (modelID, canonical string, err error) {
	err = s.q.QueryRowContext(ctx, `SELECT v.id,g.name||'/'||v.name FROM client_model_permissions x JOIN virtual_models v ON x.model_kind='virtual' AND x.model_id=v.id JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE x.client_key_id=? AND x.account_id=? AND x.enabled=1 AND g.name||'/'||v.name=?`, clientID, s.accountID, requested).Scan(&modelID, &canonical)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", sql.ErrNoRows
	}
	return modelID, canonical, err
}

func (s *Scope) VirtualRoutingMode(ctx context.Context, virtualID string) (string, error) {
	var mode string
	err := s.q.QueryRowContext(ctx, `SELECT routing_mode FROM virtual_models WHERE id=? AND account_id=?`, virtualID, s.accountID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sql.ErrNoRows
	}
	return mode, err
}

// RouteTarget is a resolved provider/model target for an inference request,
// including the credential-bearing provider fields.
type RouteTarget struct {
	ProviderModelID string
	ProviderID      string
	ProviderName    string
	ProviderType    string
	BaseURL         string
	Credential      string
	ProviderEnabled bool
	Protocols       string
	NativeProtocol  sql.NullString
	UpstreamModelID string
	Available       bool
	// Locked reports that the target's credential could not be decrypted
	// because the master key is missing or wrong. The target is marked
	// unavailable so fallback can continue, but the caller can distinguish
	// this from an ordinary outage.
	Locked                bool
	ReasoningCapabilities sql.NullString
	MaxOutputTokens       sql.NullInt64
}

func (s *Scope) VirtualRouteTargets(ctx context.Context, virtualID string) ([]RouteTarget, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT m.id,p.id,p.name,p.type,p.base_url,coalesce(p.credential_secret,''),p.enabled,p.protocols,m.native_protocol,m.upstream_model_id,m.available,m.reasoning_capabilities,m.max_output_tokens FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=? AND t.account_id=? AND m.account_id=? AND p.account_id=? AND t.enabled=1 ORDER BY t.position`, virtualID, s.accountID, s.accountID, s.accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RouteTarget{}
	for rows.Next() {
		var v RouteTarget
		var enabled, available int
		if err := rows.Scan(&v.ProviderModelID, &v.ProviderID, &v.ProviderName, &v.ProviderType, &v.BaseURL, &v.Credential, &enabled, &v.Protocols, &v.NativeProtocol, &v.UpstreamModelID, &available, &v.ReasoningCapabilities, &v.MaxOutputTokens); err != nil {
			return nil, err
		}
		v.ProviderEnabled = enabled != 0
		v.Available = enabled != 0 && available != 0
		credential, derr := s.decryptSecret(secretAAD(s.accountID, "provider", v.ProviderID, "credential_secret"), v.Credential)
		if derr != nil {
			if errors.Is(derr, ErrSecretsLocked) {
				v.Credential = ""
				v.Available = false
				v.Locked = true
			} else {
				return nil, derr
			}
		} else {
			v.Credential = credential
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Scope) RealRouteTarget(ctx context.Context, modelID string) (RouteTarget, error) {
	var v RouteTarget
	var enabled, available int
	err := s.q.QueryRowContext(ctx, `SELECT m.id,p.id,p.name,p.type,p.base_url,coalesce(p.credential_secret,''),p.enabled,p.protocols,m.native_protocol,m.upstream_model_id,m.available,m.reasoning_capabilities,m.max_output_tokens FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE m.id=? AND m.account_id=? AND p.account_id=?`, modelID, s.accountID, s.accountID).
		Scan(&v.ProviderModelID, &v.ProviderID, &v.ProviderName, &v.ProviderType, &v.BaseURL, &v.Credential, &enabled, &v.Protocols, &v.NativeProtocol, &v.UpstreamModelID, &available, &v.ReasoningCapabilities, &v.MaxOutputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return RouteTarget{}, sql.ErrNoRows
	}
	if err != nil {
		return RouteTarget{}, err
	}
	v.ProviderEnabled = enabled != 0
	v.Available = enabled != 0 && available != 0
	credential, derr := s.decryptSecret(secretAAD(s.accountID, "provider", v.ProviderID, "credential_secret"), v.Credential)
	if derr != nil {
		if errors.Is(derr, ErrSecretsLocked) {
			v.Credential = ""
			v.Available = false
			v.Locked = true
			return v, nil
		}
		return RouteTarget{}, derr
	}
	v.Credential = credential
	return v, nil
}
