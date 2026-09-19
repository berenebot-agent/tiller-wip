package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/store"
)

// logRow is the metadata captured for a single routed request. It is built up
// as the request progresses and written once, synchronously, before the
// handler returns.
type logRow struct {
	accountID                string
	clientKeyID              string
	clientName               string
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
	routeStatus              string
	requestBody              *string
	requestBodyTruncated     bool
	errorBody                *string
	errorBodyTruncated       bool
	createdAt                string
}

type requestAttempt struct {
	providerModelID, provider, model, result, failureClass string
	httpStatus                                             int
	latencyMs                                              int64
	errorMessage                                           *string
	errorBody                                              *string
	errorBodyTruncated                                     bool
	readCause                                              string
	clientCtxErr                                           string
	attemptTimedOut                                        bool
	upstreamStreaming                                      bool
	headerLatencyMs                                        int64
	// clientError is the sanitized, client-facing detail for a failed attempt
	// (provider error message/code/param). It is never persisted or exposed via
	// Activity; it exists only to build a client error response.
	clientError string
}

const maxLoggedBodyBytes = 1 << 20

func loggedBody(body []byte) (*string, bool) {
	truncated := len(body) > maxLoggedBodyBytes
	if truncated {
		body = body[:maxLoggedBodyBytes]
	}
	value := string(body)
	return &value, truncated
}

// writeLog persists a request log row. It is best-effort: a failed insert logs
// nothing and never fails the request. Logging is skipped entirely when the
// client key has logging disabled.
func (s *Server) writeLog(ctx context.Context, row *logRow) {
	if row == nil {
		return
	}
	routeStatus := row.routeStatus
	if routeStatus == "" {
		routeStatus = "legacy"
	}
	// Phase 1 local mode has exactly one account; a row built without an
	// explicit account (e.g. an older code path) belongs to it.
	if row.accountID == "" {
		row.accountID = database.LocalAccountID
	}
	s.recordLastOutcome(row)
	enabled, err := s.scopeFor(row.accountID).ClientKeyLoggingEnabled(ctx, row.clientKeyID)
	if err != nil || !enabled {
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
	attempts := make([]store.RequestAttemptInsert, 0, len(row.attempts))
	for _, attempt := range row.attempts {
		attempts = append(attempts, store.RequestAttemptInsert{
			Provider:           attempt.provider,
			Model:              attempt.model,
			Result:             attempt.result,
			HTTPStatus:         attempt.httpStatus,
			FailureClass:       attempt.failureClass,
			ErrorMessage:       attempt.errorMessage,
			ErrorBody:          attempt.errorBody,
			ErrorBodyTruncated: attempt.errorBodyTruncated,
			LatencyMs:          attempt.latencyMs,
		})
	}
	// One transaction per account batch: the request_logs row and all of its
	// attempt rows commit together or not at all. When the asynchronous writer
	// is running the row is queued and the request path never waits on the
	// commit; tests and direct Server construction fall back to a synchronous
	// best-effort insert.
	insert := store.RequestLogInsert{
		ID:                       row.clientRequestID,
		ClientKeyID:              row.clientKeyID,
		ClientName:               row.clientName,
		RequestedModel:           row.requestedModel,
		ExposedModel:             row.exposedModel,
		RouteKind:                row.routeKind,
		RouteModelID:             row.routeModelID,
		RouteModel:               row.routeModel,
		RouteStatus:              routeStatus,
		ResolvedProvider:         row.resolvedProvider,
		ResolvedModel:            row.resolvedModel,
		Protocol:                 row.protocol,
		Streaming:                row.streaming,
		HTTPStatus:               row.httpStatus,
		LatencyMs:                row.latencyMs,
		InputTokens:              row.inputTokens,
		OutputTokens:             row.outputTokens,
		CacheReadInputTokens:     row.cacheReadInputTokens,
		CacheCreationInputTokens: row.cacheCreationInputTokens,
		ProviderRequestID:        row.providerRequestID,
		ClientRequestID:          row.clientRequestID,
		ErrorText:                row.errorText,
		ErrorMessage:             row.errorMessage,
		RequestBody:              row.requestBody,
		RequestBodyTruncated:     row.requestBodyTruncated,
		ErrorBody:                row.errorBody,
		ErrorBodyTruncated:       row.errorBodyTruncated,
		FallbackUsed:             row.fallbackUsed,
		FallbackReason:           row.fallbackReason,
		CreatedAt:                row.createdAt,
		Attempts:                 attempts,
	}
	if s.logWriter != nil {
		s.logWriter.enqueue(logWrite{accountID: row.accountID, row: insert})
		return
	}
	_ = s.scopeFor(row.accountID).InsertRequestLog(ctx, insert)
}

// recordLastOutcome updates operational target status from actual attempts.
// Every attempted or skipped target gets an explicit outcome in the live
// delta so the graph can colour it. Target health is per-target: a genuine
// upstream failure degrades the target even when a later fallback rescues the
// request. Request health (whether the logical request was served) is a
// separate concept and is not recorded here. Skipped targets were never
// called and client-caused failures say nothing about the target, so neither
// degrades it.
func (s *Server) recordLastOutcome(row *logRow) {
	if len(row.attempts) == 0 {
		return
	}
	accountID := rowAccountID(row)
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
		// A failure caused by the client ending the request (cancel/timeout)
		// says nothing about the target. Never let it degrade the target or
		// show up as a failed leg.
		if attempt.result != "success" && clientCausedFailure(attempt) {
			continue
		}
		out := lastOutcome{At: recordedAt, Status: attempt.httpStatus, Result: attempt.result, FailureClass: attempt.failureClass}
		switch attempt.result {
		case "success":
			out.IsSuccess = true
			out.Degrading = true
		case "failed":
			// Preserve zero: a network failure has no HTTP response, even if a
			// later fallback succeeds and sets the logical row status to 2xx.
			out.IsSuccess = false
			// A genuine upstream failure is a target-health signal on its own,
			// independent of whether the logical request was ultimately served
			// by a fallback.
			out.Degrading = true
		case "skipped":
			// A skipped target was never called. It shows as an amber leg on
			// the graph but never paints the target unhealthy on the main page.
			out.IsSuccess = false
			out.Degrading = false
		default:
			continue
		}
		delta[attempt.providerModelID] = out
		if out.Degrading {
			s.lastOutcome[tenantKey(accountID, attempt.providerModelID)] = out
		}
	}
	s.lastOutcomeMu.Unlock()
	// Push the changed outcomes to live subscribers. Non-blocking: a full
	// buffer drops the delta, which the next snapshot self-heals. Never blocks
	// the inference path.
	if len(delta) > 0 && s.liveHub != nil {
		s.liveHub.emitOutcome(accountID, delta)
	}
}

// clientCausedFailure reports whether an attempt failed because the client
// ended the request rather than because the target misbehaved. The request
// context error is captured on the attempt for exactly this distinction.
func clientCausedFailure(attempt requestAttempt) bool {
	if attempt.failureClass == "client_cancelled" || attempt.failureClass == "client_timeout" {
		return true
	}
	return attempt.clientCtxErr != ""
}

// pruneRequestLogs deletes request logs older than each client's retention
// window. Runs at startup and hourly.
func (s *Server) pruneRequestLogs(ctx context.Context) {
	_ = s.storeHandle().PruneRequestLogs(ctx, time.Now())
	s.invalidateAllUsageAggregates()
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
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return body
	}
	model, ok := object["model"]
	if !ok {
		return body
	}
	var value string
	if err := json.Unmarshal(model, &value); err != nil || value != upstream {
		return body
	}
	replacement, err := json.Marshal(requested)
	if err != nil {
		return body
	}
	object["model"] = replacement
	result, err := json.Marshal(object)
	if err != nil {
		return body
	}
	return result
}

// newRequestID generates the router-owned request ID returned to the client.
func newRequestID() string {
	if v, err := id.New(); err == nil {
		return v
	}
	return fmt.Sprintf("req_%d", time.Now().UnixNano())
}

func strPtr(s string) *string { return &s }
