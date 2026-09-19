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
// transaction under the scope's account.
func (s *Scope) InsertRequestLog(ctx context.Context, in RequestLogInsert) error {
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
	return s.RunTx(ctx, nil, func(tx *Scope) error {
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO request_logs(id,account_id,client_key_id,requested_model,exposed_model,route_kind,route_model_id,route_model,route_status,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,provider_request_id,client_request_id,error_text,error_message,request_body,request_body_truncated,error_body,error_body_truncated,attempt_count,fallback_used,fallback_reason,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			in.ID, tx.accountID, in.ClientKeyID, in.RequestedModel, in.ExposedModel, in.RouteKind, in.RouteModelID, in.RouteModel, routeStatus, in.ResolvedProvider, in.ResolvedModel, in.Protocol, boolInt(in.Streaming), in.HTTPStatus, in.LatencyMs, in.InputTokens, in.OutputTokens, in.CacheReadInputTokens, in.CacheCreationInputTokens, in.ProviderRequestID, in.ClientRequestID, in.ErrorText, in.ErrorMessage, in.RequestBody, boolInt(in.RequestBodyTruncated), in.ErrorBody, boolInt(in.ErrorBodyTruncated), attempts, boolInt(in.FallbackUsed), in.FallbackReason, in.CreatedAt); err != nil {
			return err
		}
		for i, attempt := range in.Attempts {
			attemptID, err := id.New()
			if err != nil {
				continue
			}
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO request_attempts(id,account_id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,error_message,error_body,error_body_truncated,latency_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, attemptID, tx.accountID, in.ID, i+1, attempt.Provider, attempt.Model, attempt.Result, nullIntValue(attempt.HTTPStatus), nullStringValue(attempt.FailureClass), attempt.ErrorMessage, attempt.ErrorBody, boolInt(attempt.ErrorBodyTruncated), attempt.LatencyMs, in.CreatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// PruneRequestLogs deletes request logs older than each client key's retention
// window. It is platform-level maintenance and deliberately spans all
// accounts, so it lives on Store rather than an account Scope.
func (s *Store) PruneRequestLogs(ctx context.Context, now time.Time) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT retention_days FROM client_keys`)
	if err != nil {
		return err
	}
	var days []int
	for rows.Next() {
		var d int
		if rows.Scan(&d) == nil {
			days = append(days, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range days {
		cutoff := now.UTC().Add(-time.Duration(d) * 24 * time.Hour).Format(time.RFC3339Nano)
		if _, err := s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE client_key_id IN (SELECT id FROM client_keys WHERE retention_days=?) AND created_at < ?`, d, cutoff); err != nil {
			return fmt.Errorf("prune request logs (retention %d): %w", d, err)
		}
	}
	return nil
}
