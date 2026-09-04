package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/providers"
)

// logRow is the metadata captured for a single routed request. It is built up
// as the request progresses and written once, synchronously, before the
// handler returns. Only metadata is ever stored — never prompt/response bodies,
// tool arguments, reasoning content, or credentials.
type logRow struct {
	clientKeyID              string
	requestedModel           string
	exposedModel             *string
	routeKind                *string
	routeModelID             *string
	routeModel               *string
	resolvedProvider         *string
	resolvedModel            *string
	protocol                 string
	streaming                bool
	httpStatus               int
	latencyMs                int64
	inputTokens              *int64
	outputTokens             *int64
	cacheReadInputTokens     *int64
	cacheCreationInputTokens *int64
	providerRequestID        *string
	clientRequestID          string
	errorText                *string
	errorMessage             *string
	fallbackUsed             bool
	fallbackReason           *string
	attempts                 []requestAttempt
	createdAt                string
}

type requestAttempt struct {
	providerModelID, provider, model, result, failureClass string
	httpStatus                                             int
	latencyMs                                              int64
	errorMessage                                           *string
}

// writeLog persists a request log row. It is best-effort: a failed insert logs
// nothing and never fails the request. Logging is skipped entirely when the
// client key has logging disabled.
func (s *Server) writeLog(ctx context.Context, row *logRow) {
	if row == nil {
		return
	}
	s.recordLastOutcome(row)
	var enabled int
	if err := s.db.SQL.QueryRowContext(ctx, `SELECT logging_enabled FROM client_keys WHERE id=?`, row.clientKeyID).Scan(&enabled); err != nil || enabled == 0 {
		return
	}
	// Write-time invariant: a 2xx "success" row must always carry a resolved
	// target. Log a warning (never fail the request) if it does not, so a future
	// code path that forgets to set resolved_provider/resolved_model surfaces
	// early instead of silently writing an unattributable success row.
	if row.httpStatus >= 200 && row.httpStatus < 300 && (row.resolvedProvider == nil || row.resolvedModel == nil) {
		if s.logger != nil {
			s.logger.Warn("request logged as success without a resolved target", "client_request_id", row.clientRequestID, "requested_model", row.requestedModel, "http_status", row.httpStatus)
		}
	}
	// One transaction per logical request: the request_logs row and all of its
	// attempt rows commit together or not at all, so a single SQLite fsync (the
	// implicit-transaction commit) covers the whole write instead of 1+N.
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback() // no-op after a successful Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO request_logs(id,client_key_id,requested_model,exposed_model,route_kind,route_model_id,route_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,provider_request_id,client_request_id,error_text,error_message,attempt_count,fallback_used,fallback_reason,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.clientRequestID, row.clientKeyID, row.requestedModel, row.exposedModel, row.routeKind, row.routeModelID, row.routeModel, row.resolvedProvider, row.resolvedModel, row.protocol, boolInt(row.streaming), row.httpStatus, row.latencyMs, row.inputTokens, row.outputTokens, row.cacheReadInputTokens, row.cacheCreationInputTokens, row.providerRequestID, row.clientRequestID, row.errorText, row.errorMessage, max(1, len(row.attempts)), boolInt(row.fallbackUsed), row.fallbackReason, row.createdAt); err != nil {
		return
	}
	for i, attempt := range row.attempts {
		attemptID, err := id.New()
		if err != nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO request_attempts(id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,error_message,latency_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, attemptID, row.clientRequestID, i+1, attempt.provider, attempt.model, attempt.result, nullInt(attempt.httpStatus), nullString(attempt.failureClass), attempt.errorMessage, attempt.latencyMs, row.createdAt); err != nil {
			return
		}
	}
	_ = tx.Commit()
}

// recordLastOutcome updates operational target status from actual attempts.
// Skipped targets were not called and therefore do not receive an outcome.
func (s *Server) recordLastOutcome(row *logRow) {
	if len(row.attempts) == 0 {
		return
	}
	s.lastOutcomeMu.Lock()
	if s.lastOutcome == nil {
		s.lastOutcome = map[string]lastOutcome{}
	}
	recordedAt := database.Now()
	delta := make(map[string]lastOutcome, len(row.attempts))
	for _, attempt := range row.attempts {
		if attempt.providerModelID == "" {
			continue
		}
		switch attempt.result {
		case "success":
			s.lastOutcome[attempt.providerModelID] = lastOutcome{At: recordedAt, Status: attempt.httpStatus, IsSuccess: true}
			delta[attempt.providerModelID] = lastOutcome{At: recordedAt, Status: attempt.httpStatus, IsSuccess: true}
		case "failed":
			// Preserve zero: a network failure has no HTTP response, even if a
			// later fallback succeeds and sets the logical row status to 2xx.
			s.lastOutcome[attempt.providerModelID] = lastOutcome{At: recordedAt, Status: attempt.httpStatus, IsSuccess: false}
			delta[attempt.providerModelID] = lastOutcome{At: recordedAt, Status: attempt.httpStatus, IsSuccess: false}
		}
	}
	s.lastOutcomeMu.Unlock()
	// Push the changed outcomes to live subscribers. Non-blocking: a full
	// buffer drops the delta, which the next snapshot self-heals. Never blocks
	// the inference path.
	if len(delta) > 0 && s.liveHub != nil {
		s.liveHub.emitOutcome(delta)
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
	inputTokens              *int64
	outputTokens             *int64
	cacheReadInputTokens     *int64 // OpenAI cached_tokens / Anthropic cache_read_input_tokens
	cacheCreationInputTokens *int64 // Anthropic cache_creation_input_tokens
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
	setCacheFromUsage(u, usage)
}

// captureStreamUsage extracts usage from a single SSE event payload, handling
// the shape each upstream protocol uses.
func captureStreamUsage(payload map[string]any, target providers.Protocol, usage *usageCapture) {
	switch target {
	case providers.ProtocolChat:
		if u, ok := payload["usage"].(map[string]any); ok {
			setUsage(usage, u["prompt_tokens"], u["completion_tokens"])
			setCacheFromUsage(u, usage)
		}
	case providers.ProtocolMessages:
		if u, ok := payload["usage"].(map[string]any); ok {
			setUsage(usage, nil, u["output_tokens"])
			setCacheFromUsage(u, usage)
		}
		if msg, ok := payload["message"].(map[string]any); ok {
			if u, ok := msg["usage"].(map[string]any); ok {
				setUsage(usage, u["input_tokens"], nil)
				setCacheFromUsage(u, usage)
			}
		}
	case providers.ProtocolResponses:
		if resp, ok := payload["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				setUsage(usage, u["input_tokens"], u["output_tokens"])
				setCacheFromUsage(u, usage)
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

// setCacheFromUsage records provider-reported prompt-cache token fields:
// OpenAI-style cached_tokens (chat prompt_tokens_details / Responses
// input_tokens_details), DeepSeek's native prompt_cache_hit_tokens, and
// Anthropic cache_read/cache_creation input tokens. Only numbers are
// retained; first non-nil wins.
func setCacheFromUsage(u map[string]any, usage *usageCapture) {
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := intVal(d["cached_tokens"]); ok && usage.cacheReadInputTokens == nil {
			usage.cacheReadInputTokens = v
		}
	}
	if d, ok := u["input_tokens_details"].(map[string]any); ok {
		if v, ok := intVal(d["cached_tokens"]); ok && usage.cacheReadInputTokens == nil {
			usage.cacheReadInputTokens = v
		}
	}
	if v, ok := intVal(u["prompt_cache_hit_tokens"]); ok && usage.cacheReadInputTokens == nil {
		usage.cacheReadInputTokens = v
	}
	if v, ok := intVal(u["cache_read_input_tokens"]); ok && usage.cacheReadInputTokens == nil {
		usage.cacheReadInputTokens = v
	}
	if v, ok := intVal(u["cache_creation_input_tokens"]); ok && usage.cacheCreationInputTokens == nil {
		usage.cacheCreationInputTokens = v
	}
}

// intVal converts a JSON number to an int64 pointer. Only float64 (how
// encoding/json decodes numbers) is accepted.
func intVal(v any) (*int64, bool) {
	f, ok := v.(float64)
	if !ok {
		return nil, false
	}
	x := int64(f)
	return &x, true
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
