package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/providers"
)

// logRow is the metadata captured for a single routed request. It is built up
// as the request progresses and written once, synchronously, before the
// handler returns. Only metadata is ever stored — never prompt/response bodies,
// tool arguments, reasoning content, or credentials.
type logRow struct {
	clientKeyID       string
	requestedModel    string
	resolvedProvider  *string
	resolvedModel     *string
	protocol          string
	streaming         bool
	httpStatus        int
	latencyMs         int64
	inputTokens       *int64
	outputTokens      *int64
	providerRequestID *string
	clientRequestID   string
	errorText         *string
	fallbackUsed      bool
	fallbackReason    *string
	attempts          []requestAttempt
	createdAt         string
}

type requestAttempt struct {
	provider, model, result, failureClass string
	httpStatus                            int
	latencyMs                             int64
}

// writeLog persists a request log row. It is best-effort: a failed insert logs
// nothing and never fails the request. Logging is skipped entirely when the
// client key has logging disabled.
func (s *Server) writeLog(ctx context.Context, row *logRow) {
	if row == nil {
		return
	}
	var enabled int
	if err := s.db.SQL.QueryRowContext(ctx, `SELECT logging_enabled FROM client_keys WHERE id=?`, row.clientKeyID).Scan(&enabled); err != nil || enabled == 0 {
		return
	}
	_, _ = s.db.SQL.ExecContext(ctx, `INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,provider_request_id,client_request_id,error_text,attempt_count,fallback_used,fallback_reason,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.clientRequestID, row.clientKeyID, row.requestedModel, row.resolvedProvider, row.resolvedModel, row.protocol, boolInt(row.streaming), row.httpStatus, row.latencyMs, row.inputTokens, row.outputTokens, row.providerRequestID, row.clientRequestID, row.errorText, max(1, len(row.attempts)), boolInt(row.fallbackUsed), row.fallbackReason, row.createdAt)
	for i, attempt := range row.attempts {
		attemptID, err := id.New()
		if err != nil {
			continue
		}
		_, _ = s.db.SQL.ExecContext(ctx, `INSERT INTO request_attempts(id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,latency_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, attemptID, row.clientRequestID, i+1, attempt.provider, attempt.model, attempt.result, nullInt(attempt.httpStatus), nullString(attempt.failureClass), attempt.latencyMs, row.createdAt)
	}
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// pruneRequestLogs deletes request logs older than each client's retention
// window. Runs at startup and hourly.
func (s *Server) pruneRequestLogs(ctx context.Context) {
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT DISTINCT retention_days FROM client_keys`)
	if err != nil {
		return
	}
	var days []int
	for rows.Next() {
		var d int
		if rows.Scan(&d) == nil {
			days = append(days, d)
		}
	}
	rows.Close()
	for _, d := range days {
		cutoff := time.Now().UTC().Add(-time.Duration(d) * 24 * time.Hour).Format(time.RFC3339Nano)
		_, _ = s.db.SQL.ExecContext(ctx, `DELETE FROM request_logs WHERE client_key_id IN (SELECT id FROM client_keys WHERE retention_days=?) AND created_at < ?`, d, cutoff)
	}
}

// usageCapture accumulates token counts extracted from a response body in
// memory. Only the numbers are ever retained; the body is discarded.
type usageCapture struct {
	inputTokens  *int64
	outputTokens *int64
}

// extractUsage parses a non-streaming JSON response body for usage numbers.
func extractUsage(body []byte, usage *usageCapture) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return
	}
	u, ok := payload["usage"].(map[string]any)
	if !ok {
		return
	}
	setUsage(usage, u["prompt_tokens"], u["completion_tokens"])
	setUsage(usage, u["input_tokens"], u["output_tokens"])
}

// captureStreamUsage extracts usage from a single SSE event payload, handling
// the shape each upstream protocol uses.
func captureStreamUsage(payload map[string]any, target providers.Protocol, usage *usageCapture) {
	switch target {
	case providers.ProtocolChat:
		if u, ok := payload["usage"].(map[string]any); ok {
			setUsage(usage, u["prompt_tokens"], u["completion_tokens"])
		}
	case providers.ProtocolMessages:
		if u, ok := payload["usage"].(map[string]any); ok {
			setUsage(usage, nil, u["output_tokens"])
		}
		if msg, ok := payload["message"].(map[string]any); ok {
			if u, ok := msg["usage"].(map[string]any); ok {
				setUsage(usage, u["input_tokens"], nil)
			}
		}
	case providers.ProtocolResponses:
		if resp, ok := payload["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				setUsage(usage, u["input_tokens"], u["output_tokens"])
			}
		}
	}
}

// setUsage records the first non-nil input/output token count it sees.
func setUsage(usage *usageCapture, input, output any) {
	if in, ok := input.(float64); ok && usage.inputTokens == nil {
		v := int64(in)
		usage.inputTokens = &v
	}
	if out, ok := output.(float64); ok && usage.outputTokens == nil {
		v := int64(out)
		usage.outputTokens = &v
	}
}

// rewriteModelBytes replaces the upstream model identifier in a non-streaming
// JSON body with the client-facing requested model.
func rewriteModelBytes(body []byte, upstream, requested string) []byte {
	body = bytes.ReplaceAll(body, []byte(`"model":"`+upstream+`"`), []byte(`"model":"`+requested+`"`))
	body = bytes.ReplaceAll(body, []byte(`"model": "`+upstream+`"`), []byte(`"model": "`+requested+`"`))
	return body
}

// newRequestID generates the router-owned request ID returned to the client.
func newRequestID() string {
	if v, err := id.New(); err == nil {
		return v
	}
	return fmt.Sprintf("req_%d", time.Now().UnixNano())
}

func strPtr(s string) *string { return &s }
