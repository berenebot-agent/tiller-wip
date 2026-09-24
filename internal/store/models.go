package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/id"
)

var (
	// ErrManualModelExists is returned when a manual model would duplicate an
	// existing (provider_id, upstream_model_id) row.
	ErrManualModelExists = errors.New("model already exists for provider")
	ErrModelNotFound     = errors.New("store: model not found")
	ErrModelNotManual    = errors.New("store: model is not manually added")
	ErrModelInUse        = errors.New("store: model is referenced")
)

// CatalogueModel is a discovered or manually-added model to persist.
type CatalogueModel struct {
	ID                       string
	DisplayName              string
	ContextLength            *int
	MaxOutputTokens          *int
	NativeProtocol           string
	SupportsTools            *bool
	SupportsVision           *bool
	SupportsReasoning        *bool
	SupportsStructuredOutput *bool
	InputModalities          []string
	OutputModalities         []string
	ReasoningCapabilities    json.RawMessage
}

const providerModelInsertColumns = "id,account_id,provider_id,upstream_model_id,display_name,context_length,max_output_tokens,native_protocol,supports_tools,supports_vision,supports_reasoning,supports_structured_output,input_modalities,output_modalities,reasoning_capabilities,origin,available,first_seen_at,last_seen_at,created_at,updated_at"

const providerModelInsertPlaceholders = "(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?)"

const providerModelInsertArgs = 20

func providerModelArgs(modelID, accountID, providerID string, model CatalogueModel, origin, now string) []any {
	return []any{
		modelID, accountID, providerID, model.ID, model.DisplayName,
		nullableInt(model.ContextLength), nullableInt(model.MaxOutputTokens),
		nullableProtocol(model.NativeProtocol),
		nullableBool(model.SupportsTools), nullableBool(model.SupportsVision),
		nullableBool(model.SupportsReasoning), nullableBool(model.SupportsStructuredOutput),
		nullableJSON(model.InputModalities), nullableJSON(model.OutputModalities),
		nullableRawJSON(model.ReasoningCapabilities),
		origin, now, now, now, now,
	}
}

// ApplyCatalogue upserts a provider's discovered models, seeds permissions for
// new ones, retires vanished ones, and records the next refresh time — all
// within the account.
func (s *Scope) ApplyCatalogue(ctx context.Context, providerID string, models []CatalogueModel) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		now := now()
		unique := make([]CatalogueModel, 0, len(models))
		seen := make(map[string]bool, len(models))
		for _, model := range models {
			if model.ID == "" || seen[model.ID] {
				continue
			}
			seen[model.ID] = true
			unique = append(unique, model)
		}

		existing := make(map[string]string, len(unique))
		rows, err := tx.q.QueryContext(ctx, `SELECT id,upstream_model_id FROM provider_models WHERE provider_id=? AND account_id=?`, providerID, tx.accountID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var modelID, upstream string
			if err := rows.Scan(&modelID, &upstream); err != nil {
				rows.Close()
				return err
			}
			existing[upstream] = modelID
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		ids := make([]string, len(unique))
		newIDs := make([]string, 0, len(unique))
		for i, model := range unique {
			if modelID, ok := existing[model.ID]; ok {
				ids[i] = modelID
				continue
			}
			newID, err := id.New()
			if err != nil {
				return err
			}
			ids[i] = newID
			newIDs = append(newIDs, newID)
		}

		if len(unique) > 0 {
			const upsertBatchRows = 50
			for start := 0; start < len(unique); start += upsertBatchRows {
				end := start + upsertBatchRows
				if end > len(unique) {
					end = len(unique)
				}
				placeholders := make([]string, 0, end-start)
				args := make([]any, 0, (end-start)*providerModelInsertArgs)
				for i := start; i < end; i++ {
					placeholders = append(placeholders, providerModelInsertPlaceholders)
					args = append(args, providerModelArgs(ids[i], tx.accountID, providerID, unique[i], "discovered", now)...)
				}
				stmt := `INSERT INTO provider_models(` + providerModelInsertColumns + `) VALUES ` +
					strings.Join(placeholders, ",") + `
ON CONFLICT(provider_id, upstream_model_id) DO UPDATE SET
display_name=excluded.display_name,
context_length=excluded.context_length,
max_output_tokens=excluded.max_output_tokens,
native_protocol=excluded.native_protocol,
supports_tools=excluded.supports_tools,
supports_vision=excluded.supports_vision,
supports_reasoning=excluded.supports_reasoning,
supports_structured_output=excluded.supports_structured_output,
input_modalities=excluded.input_modalities,
output_modalities=excluded.output_modalities,
reasoning_capabilities=excluded.reasoning_capabilities,
available=1,
last_seen_at=excluded.last_seen_at,
updated_at=excluded.updated_at,
origin=excluded.origin`
				if _, err := tx.q.ExecContext(ctx, stmt, args...); err != nil {
					return err
				}
			}
		}

		for _, newID := range newIDs {
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at,account_id)
SELECT c.id,'real',?,coalesce(d.new_models_enabled,0),?,?,? FROM client_keys c LEFT JOIN client_group_defaults d ON d.client_key_id=c.id AND d.group_kind='real' AND d.group_id=? AND d.account_id=? WHERE c.account_id=?`, newID, now, now, tx.accountID, providerID, tx.accountID, tx.accountID); err != nil {
				return err
			}
		}

		if len(seen) > 0 {
			rows, qerr := tx.q.QueryContext(ctx, `SELECT upstream_model_id FROM provider_models WHERE provider_id=? AND account_id=? AND available=1 AND origin='discovered'`, providerID, tx.accountID)
			if qerr != nil {
				return qerr
			}
			var stale []string
			for rows.Next() {
				var upstream string
				if serr := rows.Scan(&upstream); serr != nil {
					rows.Close()
					return serr
				}
				if !seen[upstream] {
					stale = append(stale, upstream)
				}
			}
			rows.Close()
			if rerr := rows.Err(); rerr != nil {
				return rerr
			}
			const retireBatch = 500
			for start := 0; start < len(stale); start += retireBatch {
				end := start + retireBatch
				if end > len(stale) {
					end = len(stale)
				}
				placeholders := make([]string, 0, end-start)
				rargs := make([]any, 0, (end-start)+3)
				rargs = append(rargs, now, providerID, tx.accountID)
				for _, u := range stale[start:end] {
					placeholders = append(placeholders, "?")
					rargs = append(rargs, u)
				}
				if _, uerr := tx.q.ExecContext(ctx, `UPDATE provider_models SET available=0,updated_at=? WHERE provider_id=? AND account_id=? AND available=1 AND origin='discovered' AND upstream_model_id IN (`+strings.Join(placeholders, ",")+`)`, rargs...); uerr != nil {
					return uerr
				}
			}
		} else {
			if _, err := tx.q.ExecContext(ctx, `UPDATE provider_models SET available=0,updated_at=? WHERE provider_id=? AND account_id=? AND available=1 AND origin='discovered'`, now, providerID, tx.accountID); err != nil {
				return err
			}
		}

		next := time.Now().UTC().Add(24*time.Hour + RefreshJitter(providerID)).Format(time.RFC3339Nano)
		if _, err := tx.q.ExecContext(ctx, `UPDATE providers SET last_refresh_at=?,next_refresh_at=?,last_refresh_error=NULL,updated_at=? WHERE id=? AND account_id=?`, now, next, now, providerID, tx.accountID); err != nil {
			return err
		}
		return nil
	})
}

// InsertManualModel persists a manually-added model and seeds its permissions.
// It returns ErrManualModelExists on a duplicate.
func (s *Scope) InsertManualModel(ctx context.Context, providerID, modelID string, model CatalogueModel) error {
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		now := now()
		result, err := tx.q.ExecContext(ctx, `INSERT INTO provider_models(`+providerModelInsertColumns+`) VALUES `+providerModelInsertPlaceholders+` ON CONFLICT(provider_id, upstream_model_id) DO NOTHING`, providerModelArgs(modelID, tx.accountID, providerID, model, "manual", now)...)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrManualModelExists
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at,account_id)
SELECT c.id,'real',?,coalesce(d.new_models_enabled,0),?,?,? FROM client_keys c LEFT JOIN client_group_defaults d ON d.client_key_id=c.id AND d.group_kind='real' AND d.group_id=? AND d.account_id=? WHERE c.account_id=?`, modelID, now, now, tx.accountID, providerID, tx.accountID, tx.accountID); err != nil {
			return err
		}
		return nil
	})
}

// ModelFilter narrows ListModels.
type ModelFilter struct {
	ProviderID string
	Search     string
	Limit      int
	Offset     int
}

// ModelRow is one row of the admin model list.
type ModelRow struct {
	ID                       string
	ProviderID               string
	ProviderName             string
	UpstreamModelID          string
	CanonicalModelID         string
	DisplayName              string
	ContextLength            *int64
	MaxOutputTokens          *int64
	NativeProtocol           sql.NullString
	SupportsTools            sql.NullInt64
	SupportsVision           sql.NullInt64
	SupportsReasoning        sql.NullInt64
	SupportsStructuredOutput sql.NullInt64
	ReasoningCapabilities    sql.NullString
	InputModalities          sql.NullString
	OutputModalities         sql.NullString
	Available                bool
	FirstSeenAt              string
	LastSeenAt               string
	Origin                   string
}

func (s *Scope) ListModels(ctx context.Context, filter ModelFilter) ([]ModelRow, error) {
	query := `SELECT m.id,m.provider_id,p.name,m.upstream_model_id,p.name||'/'||m.upstream_model_id,m.display_name,m.context_length,m.max_output_tokens,m.native_protocol,m.supports_tools,m.supports_vision,m.supports_reasoning,m.supports_structured_output,m.reasoning_capabilities,m.input_modalities,m.output_modalities,m.available,m.first_seen_at,m.last_seen_at,m.origin FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE m.account_id=? AND p.account_id=?`
	args := []any{s.accountID, s.accountID}
	if filter.ProviderID != "" {
		query += ` AND m.provider_id=?`
		args = append(args, filter.ProviderID)
	}
	pattern := "%" + filter.Search + "%"
	query += ` AND (m.upstream_model_id LIKE ? OR p.name LIKE ? OR p.name||'/'||m.upstream_model_id LIKE ?) ORDER BY p.name,m.upstream_model_id LIMIT ? OFFSET ?`
	args = append(args, pattern, pattern, pattern, filter.Limit, filter.Offset)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelRow{}
	for rows.Next() {
		var v ModelRow
		var available int
		if err := rows.Scan(&v.ID, &v.ProviderID, &v.ProviderName, &v.UpstreamModelID, &v.CanonicalModelID, &v.DisplayName, &v.ContextLength, &v.MaxOutputTokens, &v.NativeProtocol, &v.SupportsTools, &v.SupportsVision, &v.SupportsReasoning, &v.SupportsStructuredOutput, &v.ReasoningCapabilities, &v.InputModalities, &v.OutputModalities, &available, &v.FirstSeenAt, &v.LastSeenAt, &v.Origin); err != nil {
			return nil, err
		}
		v.Available = available != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteManualModel removes a manually-added model and its permissions. It
// returns ErrModelNotFound, ErrModelNotManual or ErrModelInUse as appropriate.
func (s *Scope) DeleteManualModel(ctx context.Context, modelID string) error {
	var origin string
	err := s.q.QueryRowContext(ctx, `SELECT origin FROM provider_models WHERE id=? AND account_id=?`, modelID, s.accountID).Scan(&origin)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrModelNotFound
	}
	if err != nil {
		return err
	}
	if origin != "manual" {
		return ErrModelNotManual
	}
	var refs int
	if err := s.q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM client_single_bindings WHERE real_model_id=? AND account_id=?) + (SELECT count(*) FROM virtual_model_targets WHERE provider_model_id=? AND account_id=?) + (SELECT count(*) FROM virtual_models WHERE target_provider_model_id=? AND account_id=?)`, modelID, s.accountID, modelID, s.accountID, modelID, s.accountID).Scan(&refs); err != nil {
		return err
	}
	if refs > 0 {
		return ErrModelInUse
	}
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM client_model_permissions WHERE model_kind='real' AND model_id=? AND account_id=?`, modelID, tx.accountID); err != nil {
			return err
		}
		_, err := tx.q.ExecContext(ctx, `DELETE FROM provider_models WHERE id=? AND origin='manual' AND account_id=?`, modelID, tx.accountID)
		return err
	})
}

// RefreshJitter returns a deterministic ±1h jitter for a provider's refresh
// schedule, seeded by provider ID.
func RefreshJitter(providerID string) time.Duration {
	var h uint32 = 2166136261
	for i := 0; i < len(providerID); i++ {
		h ^= uint32(providerID[i])
		h *= 16777619
	}
	return time.Duration(int64(h%7200)-3600) * time.Second
}

func nullableInt(v *int) any {
	if v == nil || *v <= 0 {
		return nil
	}
	return *v
}

func nullableProtocol(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableBool(v *bool) any {
	if v == nil {
		return nil
	}
	if *v {
		return 1
	}
	return 0
}

func nullableJSON(list []string) any {
	if len(list) == 0 {
		return nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return nil
	}
	return string(b)
}

func nullableRawJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
