package store

import (
	"context"
	"database/sql"
	"errors"
)

// This file owns all Activity attribution SQL (list, export, attempts). The
// attribution predicates are shared with usage aggregation so no view can
// drift.

// virtualAttribution returns the predicate + args that match request_logs rows
// attributable to a virtual model, handling both new rows (route_model_id) and
// legacy rows (route_kind NULL, matched by canonical name).
func virtualAttribution(virtualID, canonical string) (string, []any) {
	return `((rl.route_kind='virtual' AND rl.route_model_id=?) OR (rl.route_status='legacy' AND rl.route_kind IS NULL AND rl.requested_model=?) OR (rl.route_status='legacy' AND rl.route_kind IS NULL AND rl.route_model=?))`,
		[]any{virtualID, canonical, canonical}
}

// realAttribution returns the predicate + args that match rows resolved to a
// real model. Callers must alias request_logs as "rl".
func realAttribution(modelID, provider, upstream string) (string, []any) {
	return `((rl.route_kind='real' AND rl.route_model_id=?) OR (rl.route_kind='virtual' AND rl.resolved_provider=? AND rl.resolved_model=?) OR (rl.route_status='legacy' AND rl.route_kind IS NULL AND rl.resolved_provider=? AND rl.resolved_model=?) OR (rl.route_kind='virtual' AND EXISTS (SELECT 1 FROM request_attempts ra WHERE ra.request_log_id=rl.id AND ra.provider=? AND ra.model=? AND ra.result='failed')))`, []any{modelID, provider, upstream, provider, upstream, provider, upstream}
}

func activitySearchClause(alias, pattern string) (string, []any) {
	return `(` + alias + `requested_model LIKE ? OR coalesce(` + alias + `exposed_model,'') LIKE ? OR coalesce(` + alias + `route_model,'') LIKE ? OR coalesce(` + alias + `resolved_provider,'') LIKE ? OR coalesce(` + alias + `resolved_model,'') LIKE ? OR coalesce(` + alias + `resolved_provider,'') || '/' || coalesce(` + alias + `resolved_model,'') LIKE ? OR CAST(` + alias + `http_status AS TEXT) LIKE ? OR coalesce(` + alias + `client_request_id,'') LIKE ? OR coalesce(` + alias + `provider_request_id,'') LIKE ? OR coalesce(` + alias + `error_text,'') LIKE ? OR coalesce(` + alias + `error_message,'') LIKE ?)`, []any{pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern}
}

// ActivityRow is one request_logs row for the Activity views.
type ActivityRow struct {
	ID                       string
	RequestedModel           string
	ExposedModel             *string
	RouteKind                *string
	RouteModelID             *string
	RouteModel               *string
	ResolvedProvider         *string
	ResolvedModel            *string
	Protocol                 string
	Streaming                bool
	HTTPStatus               int
	LatencyMs                int64
	InputTokens              *int64
	OutputTokens             *int64
	CacheReadInputTokens     *int64
	CacheCreationInputTokens *int64
	ProviderRequestID        *string
	ClientRequestID          string
	ErrorText                *string
	ErrorMessage             *string
	RequestBody              *string
	RequestBodyTruncated     bool
	ErrorBody                *string
	ErrorBodyTruncated       bool
	AttemptCount             int
	AttemptRows              int
	FallbackUsed             bool
	FallbackReason           *string
	CreatedAt                string
	ClientKeyID              string
	ClientName               string
}

// RequestAttemptRow is one request_attempts row.
type RequestAttemptRow struct {
	AttemptNumber      int
	Provider           string
	Model              string
	Result             string
	HTTPStatus         *int
	FailureClass       *string
	ErrorMessage       *string
	ErrorBody          *string
	ErrorBodyTruncated bool
	LatencyMs          int64
	CreatedAt          string
}

const activityColumns = `rl.id,rl.requested_model,rl.exposed_model,rl.route_kind,rl.route_model_id,rl.route_model,rl.resolved_provider,rl.resolved_model,rl.protocol,rl.streaming,rl.http_status,rl.latency_ms,rl.input_tokens,rl.output_tokens,rl.cache_read_input_tokens,rl.cache_creation_input_tokens,rl.provider_request_id,rl.client_request_id,rl.error_text,rl.error_message,rl.request_body,rl.request_body_truncated,rl.error_body,rl.error_body_truncated,rl.attempt_count,(SELECT COUNT(*) FROM request_attempts ra WHERE ra.request_log_id=rl.id AND ra.account_id=rl.account_id),rl.fallback_used,rl.fallback_reason,rl.created_at`

func (s *Scope) scanActivityRows(rows *sql.Rows, withClientKey, withClientName bool) ([]ActivityRow, error) {
	defer rows.Close()
	out := []ActivityRow{}
	for rows.Next() {
		var v ActivityRow
		var streaming, fallback, bodyTruncated, errorTruncated int
		dest := []any{&v.ID, &v.RequestedModel, &v.ExposedModel, &v.RouteKind, &v.RouteModelID, &v.RouteModel, &v.ResolvedProvider, &v.ResolvedModel, &v.Protocol, &streaming, &v.HTTPStatus, &v.LatencyMs, &v.InputTokens, &v.OutputTokens, &v.CacheReadInputTokens, &v.CacheCreationInputTokens, &v.ProviderRequestID, &v.ClientRequestID, &v.ErrorText, &v.ErrorMessage, &v.RequestBody, &bodyTruncated, &v.ErrorBody, &errorTruncated, &v.AttemptCount, &v.AttemptRows, &fallback, &v.FallbackReason, &v.CreatedAt}
		if withClientKey {
			dest = append(dest, &v.ClientKeyID)
		}
		if withClientName {
			dest = append(dest, &v.ClientName)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		v.Streaming = streaming != 0
		v.FallbackUsed = fallback != 0
		v.RequestBodyTruncated = bodyTruncated != 0
		v.ErrorBodyTruncated = errorTruncated != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Scope) ClientKeyExists(ctx context.Context, id string) (bool, error) {
	var exists int
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM client_keys WHERE id=? AND account_id=?`, id, s.accountID).Scan(&exists); err != nil {
		return false, err
	}
	return exists > 0, nil
}

func (s *Scope) VirtualModelCanonical(ctx context.Context, id string) (string, error) {
	var canonical string
	err := s.q.QueryRowContext(ctx, `SELECT g.name||'/'||v.name FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE v.id=? AND v.account_id=? AND g.account_id=?`, id, s.accountID, s.accountID).Scan(&canonical)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrVirtualModelNotFound
	}
	return canonical, err
}

func (s *Scope) RealModelCanonical(ctx context.Context, id string) (provider, upstream string, err error) {
	err = s.q.QueryRowContext(ctx, `SELECT p.name,m.upstream_model_id FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE m.id=? AND m.account_id=? AND p.account_id=?`, id, s.accountID, s.accountID).Scan(&provider, &upstream)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrModelNotFound
	}
	return provider, upstream, err
}

func (s *Scope) ListClientActivity(ctx context.Context, clientID, search string, limit, offset int) ([]ActivityRow, error) {
	searchClause, searchArgs := activitySearchClause("rl.", "%"+search+"%")
	args := append([]any{s.accountID, clientID}, searchArgs...)
	args = append(args, limit, offset)
	var out []ActivityRow
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT `+activityColumns+` FROM request_logs rl WHERE rl.account_id=? AND rl.client_key_id=? AND `+searchClause+` ORDER BY rl.created_at DESC, rl.id DESC LIMIT ? OFFSET ?`, args...)
		if err != nil {
			return err
		}
		out, err = s.scanActivityRows(rows, false, false)
		return err
	})
	return out, err
}

func (s *Scope) ListGlobalActivity(ctx context.Context, search string, limit, offset int) ([]ActivityRow, error) {
	pattern := "%" + search + "%"
	searchClause, searchArgs := activitySearchClause("rl.", pattern)
	searchClause = "(rl.client_name LIKE ? OR " + searchClause + ")"
	searchArgs = append([]any{pattern}, searchArgs...)
	args := append([]any{s.accountID}, searchArgs...)
	args = append(args, limit, offset)
	var out []ActivityRow
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT `+activityColumns+`,rl.client_key_id,rl.client_name FROM request_logs rl WHERE rl.account_id=? AND `+searchClause+` ORDER BY rl.created_at DESC, rl.id DESC LIMIT ? OFFSET ?`, args...)
		if err != nil {
			return err
		}
		out, err = s.scanActivityRows(rows, true, true)
		return err
	})
	return out, err
}

func (s *Scope) ListVirtualActivity(ctx context.Context, virtualID, canonical, search string, limit, offset int) ([]ActivityRow, error) {
	where, whereArgs := virtualAttribution(virtualID, canonical)
	searchClause, searchArgs := activitySearchClause("rl.", "%"+search+"%")
	args := append([]any{s.accountID}, whereArgs...)
	args = append(args, searchArgs...)
	args = append(args, limit, offset)
	var out []ActivityRow
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT `+activityColumns+`,rl.client_key_id,rl.client_name FROM request_logs rl WHERE rl.account_id=? AND `+where+` AND `+searchClause+` ORDER BY rl.created_at DESC, rl.id DESC LIMIT ? OFFSET ?`, args...)
		if err != nil {
			return err
		}
		out, err = s.scanActivityRows(rows, true, true)
		return err
	})
	return out, err
}

func (s *Scope) ListRealModelActivity(ctx context.Context, modelID, provider, upstream, search string, limit, offset int) ([]ActivityRow, error) {
	where, whereArgs := realAttribution(modelID, provider, upstream)
	searchClause, searchArgs := activitySearchClause("rl.", "%"+search+"%")
	args := append([]any{s.accountID}, whereArgs...)
	args = append(args, searchArgs...)
	args = append(args, limit, offset)
	var out []ActivityRow
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT `+activityColumns+`,rl.client_key_id,rl.client_name FROM request_logs rl WHERE rl.account_id=? AND `+where+` AND `+searchClause+` ORDER BY rl.created_at DESC, rl.id DESC LIMIT ? OFFSET ?`, args...)
		if err != nil {
			return err
		}
		out, err = s.scanActivityRows(rows, true, true)
		return err
	})
	return out, err
}

const activityExportBatchSize = 256

func (s *Scope) exportActivity(ctx context.Context, where string, args []any, cutoff string, fn func(ActivityRow) error) error {
	if cutoff != "" {
		where += ` AND rl.created_at >= ?`
		args = append(args, cutoff)
	}
	beforeCreated, beforeID := "", ""
	for {
		batchArgs := append([]any(nil), args...)
		batchWhere := where
		if beforeCreated != "" {
			batchWhere += ` AND (rl.created_at < ? OR (rl.created_at = ? AND rl.id < ?))`
			batchArgs = append(batchArgs, beforeCreated, beforeCreated, beforeID)
		}
		batchArgs = append(batchArgs, activityExportBatchSize)
		var page []ActivityRow
		err := s.withActivity(ctx, func(q querier) error {
			rows, err := q.QueryContext(ctx, `SELECT `+activityColumns+`,rl.client_name FROM request_logs rl WHERE rl.account_id=? AND `+batchWhere+` ORDER BY rl.created_at DESC, rl.id DESC LIMIT ?`, append([]any{s.accountID}, batchArgs...)...)
			if err != nil {
				return err
			}
			page, err = s.scanActivityRows(rows, false, true)
			return err
		})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for i := range page {
			if err := fn(page[i]); err != nil {
				return err
			}
		}
		if len(page) < activityExportBatchSize {
			return nil
		}
		beforeCreated, beforeID = page[len(page)-1].CreatedAt, page[len(page)-1].ID
	}
}

// ExportClientActivity streams one client key's activity, newest first.
func (s *Scope) ExportClientActivity(ctx context.Context, clientID, search, cutoff string, fn func(ActivityRow) error) error {
	where := `rl.client_key_id=?`
	args := []any{clientID}
	if search != "" {
		searchClause, searchArgs := activitySearchClause("rl.", "%"+search+"%")
		where += ` AND ` + searchClause
		args = append(args, searchArgs...)
	}
	return s.exportActivity(ctx, where, args, cutoff, fn)
}

// ExportVirtualActivity streams activity attributable to a virtual model.
func (s *Scope) ExportVirtualActivity(ctx context.Context, virtualID, canonical, search, cutoff string, fn func(ActivityRow) error) error {
	where, args := virtualAttribution(virtualID, canonical)
	if search != "" {
		searchClause, searchArgs := activitySearchClause("rl.", "%"+search+"%")
		where += ` AND ` + searchClause
		args = append(args, searchArgs...)
	}
	return s.exportActivity(ctx, where, args, cutoff, fn)
}

// ExportRealActivity streams activity attributable to a real model.
func (s *Scope) ExportRealActivity(ctx context.Context, modelID, provider, upstream, search, cutoff string, fn func(ActivityRow) error) error {
	where, args := realAttribution(modelID, provider, upstream)
	if search != "" {
		searchClause, searchArgs := activitySearchClause("rl.", "%"+search+"%")
		where += ` AND ` + searchClause
		args = append(args, searchArgs...)
	}
	return s.exportActivity(ctx, where, args, cutoff, fn)
}

// ClearClientActivity deletes a client key's request logs.
func (s *Scope) ClearClientActivity(ctx context.Context, clientID string) error {
	return s.withActivity(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `DELETE FROM request_logs WHERE client_key_id=? AND account_id=?`, clientID, s.accountID)
		return err
	})
}

func (s *Scope) ListRequestAttempts(ctx context.Context, requestLogID string) ([]RequestAttemptRow, error) {
	var out []RequestAttemptRow
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT attempt_number,provider,model,result,http_status,failure_class,error_message,error_body,error_body_truncated,latency_ms,created_at FROM request_attempts WHERE request_log_id=? AND account_id=? ORDER BY attempt_number`, requestLogID, s.accountID)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = []RequestAttemptRow{}
		for rows.Next() {
			var v RequestAttemptRow
			var truncated int
			if err := rows.Scan(&v.AttemptNumber, &v.Provider, &v.Model, &v.Result, &v.HTTPStatus, &v.FailureClass, &v.ErrorMessage, &v.ErrorBody, &truncated, &v.LatencyMs, &v.CreatedAt); err != nil {
				return err
			}
			v.ErrorBodyTruncated = truncated != 0
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}
