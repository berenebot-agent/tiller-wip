package store

import (
	"context"
	"fmt"
	"time"

	"github.com/tiller-router/tiller-router/internal/id"
)

// RequestAttemptInsert is one upstream attempt to persist with its request log.
type RequestAttemptInsert struct {
	Provider           string
	Model              string
	Result             string
	HTTPStatus         int
	FailureClass       string
	ErrorMessage       *string
	ErrorBody          *string
	ErrorBodyTruncated bool
	LatencyMs          int64
}

// RequestLogInsert carries a completed request's metadata for persistence. The
// row is written under the scope's account regardless of any caller-supplied
// account value.
type RequestLogInsert struct {
	ID                       string
	ClientKeyID              string
	ClientName               string
	RequestedModel           string
	ExposedModel             *string
	RouteKind                *string
	RouteModelID             *string
	RouteModel               *string
	RouteStatus              string
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
	FallbackUsed             bool
	FallbackReason           *string
	CreatedAt                string
	Attempts                 []RequestAttemptInsert
}

func nullIntValue(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullStringValue(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// InsertRequestLog writes a request log and all of its attempt rows in one
// transaction in the account's Activity database, so a single request's logs
// commit together.
func (s *Scope) InsertRequestLog(ctx context.Context, in RequestLogInsert) error {
	return s.runActivityTx(ctx, func(q querier) error {
		return insertRequestLogRow(ctx, q, s.accountID, &in)
	})
}

// InsertRequestLogs writes a batch of request logs, one transaction per
// account, in the account's Activity database. It exists for the asynchronous
// Activity writer so many completed requests share a single commit.
func (s *Scope) InsertRequestLogs(ctx context.Context, rows []RequestLogInsert) error {
	if len(rows) == 0 {
		return nil
	}
	pending, err := s.activityCleanupPending(ctx, "")
	if err != nil {
		return err
	}
	if pending {
		return nil
	}
	for _, row := range rows {
		pending, err := s.activityCleanupPending(ctx, row.ClientKeyID)
		if err != nil {
			return err
		}
		if pending {
			return nil
		}
	}
	return s.runActivityTx(ctx, func(q querier) error {
		for i := range rows {
			if err := insertRequestLogRow(ctx, q, s.accountID, &rows[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// insertRequestLogRow inserts one request log and its attempts using q. It is
// shared by the single and batched insert paths so they cannot drift.
func insertRequestLogRow(ctx context.Context, q querier, accountID string, in *RequestLogInsert) error {
	routeStatus := in.RouteStatus
	if routeStatus == "" {
		routeStatus = "legacy"
	}
	attempts := 0
	for _, a := range in.Attempts {
		if a.Result != "skipped" {
			attempts++
		}
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO request_logs(id,account_id,client_key_id,client_name,requested_model,exposed_model,route_kind,route_model_id,route_model,route_status,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,provider_request_id,client_request_id,error_text,error_message,request_body,request_body_truncated,error_body,error_body_truncated,attempt_count,fallback_used,fallback_reason,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.ID, accountID, in.ClientKeyID, in.ClientName, in.RequestedModel, in.ExposedModel, in.RouteKind, in.RouteModelID, in.RouteModel, routeStatus, in.ResolvedProvider, in.ResolvedModel, in.Protocol, boolInt(in.Streaming), in.HTTPStatus, in.LatencyMs, in.InputTokens, in.OutputTokens, in.CacheReadInputTokens, in.CacheCreationInputTokens, in.ProviderRequestID, in.ClientRequestID, in.ErrorText, in.ErrorMessage, in.RequestBody, boolInt(in.RequestBodyTruncated), in.ErrorBody, boolInt(in.ErrorBodyTruncated), attempts, boolInt(in.FallbackUsed), in.FallbackReason, in.CreatedAt); err != nil {
		return err
	}
	for i, attempt := range in.Attempts {
		attemptID, err := id.New()
		if err != nil {
			continue
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO request_attempts(id,account_id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,error_message,error_body,error_body_truncated,latency_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, attemptID, accountID, in.ID, i+1, attempt.Provider, attempt.Model, attempt.Result, nullIntValue(attempt.HTTPStatus), nullStringValue(attempt.FailureClass), attempt.ErrorMessage, attempt.ErrorBody, boolInt(attempt.ErrorBodyTruncated), attempt.LatencyMs, in.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

// PruneRequestLogs deletes request logs older than each client key's retention
// window. It is platform-level maintenance and deliberately spans all
// accounts, so it lives on Store rather than an account Scope. Each account's
// prune is account-scoped SQL against the shared Activity database.
func (s *Store) PruneRequestLogs(ctx context.Context, now time.Time) error {
	if s.activity == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, account_id, retention_days FROM client_keys`)
	if err != nil {
		return err
	}
	byAccount := map[string][]retentionKey{}
	for rows.Next() {
		var id, account string
		var days int
		if err := rows.Scan(&id, &account, &days); err != nil {
			rows.Close()
			return err
		}
		byAccount[account] = append(byAccount[account], retentionKey{id: id, days: days})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for account, keys := range byAccount {
		if err := s.pruneAccountRequestLogs(ctx, account, keys, now); err != nil {
			return err
		}
	}
	return nil
}

// retentionKey pairs a client key with its configured retention window (days).
type retentionKey struct {
	id   string
	days int
}

func (s *Store) pruneAccountRequestLogs(ctx context.Context, account string, keys []retentionKey, now time.Time) error {
	tx, err := s.activity.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, key := range keys {
		cutoff := now.UTC().Add(-time.Duration(key.days) * 24 * time.Hour).Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `DELETE FROM request_logs WHERE account_id=? AND client_key_id=? AND created_at < ?`, account, key.id, cutoff); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("prune request logs (retention %d): %w", key.days, err)
		}
	}
	return tx.Commit()
}
