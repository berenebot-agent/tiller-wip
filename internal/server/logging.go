package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
	// providerType is the selected target's provider type (e.g. "openai"),
	// used only to look up models.dev pricing for the display estimate.
	providerType *string
	// providerCostMicros is a provider-reported exact cost (OpenRouter
	// usage.cost) when the upstream response carried one.
	providerCostMicros   *int64
	inputTokensEstimated bool
	inputEstimate        int64
	providerRequestID    *string
	clientRequestID      string
	errorText            *string
	errorMessage         *string
	fallbackUsed         bool
	fallbackReason       *string
	attempts             []requestAttempt
	routeStatus          string
	requestBody          *string
	requestBodyTruncated bool
	errorBody            *string
	errorBodyTruncated   bool
	createdAt            string
}

// copyUsage moves the captured token counts and any provider-reported cost from
// the in-memory usage capture onto the log row. It is the single copy point so
// every response shape (translated stream, native SSE, non-streaming) records
// the same fields.
func (row *logRow) copyUsage(usage *usageCapture) {
	if usage == nil {
		return
	}
	row.inputTokens, row.outputTokens = usage.inputTokens, usage.outputTokens
	row.cacheReadInputTokens, row.cacheCreationInputTokens = usage.cacheReadInputTokens, usage.cacheCreationInputTokens
	row.providerCostMicros = usage.providerCostMicros
	row.inputTokensEstimated = usage.inputTokensEstimated
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
	// firstOutputLatencyMs is the delay from attempt start to the first
	// client-visible assistant frame (text, reasoning, or tool). It is distinct
	// from headerLatencyMs, which only measures receipt of upstream headers.
	firstOutputLatencyMs int64
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
		// A row whose routing was not classified is explicitly unresolved; the
		// historical 'legacy' value is no longer produced.
		routeStatus = "unresolved"
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
	// Missing-usage fallback: some upstreams omit a usage block entirely, which
	// would otherwise leave input_tokens NULL and hide the request from token
	// totals. When a successful row has no provider-reported input count but a
	// body was captured, estimate it so the request still appears in the
	// aggregate. The row is flagged estimated so the UI can mark it and so it is
	// never confused with provider-reported accounting.
	if row.inputTokens == nil && row.inputEstimate > 0 && row.httpStatus >= 200 && row.httpStatus < 300 {
		if est := row.inputEstimate; est > 0 {
			v := est
			row.inputTokens = &v
			row.inputTokensEstimated = true
		}
	}
	// Estimated cost is computed only for a successfully resolved row that has
	// token counts and a resolvable provider type. A provider-reported exact
	// cost (providerCostMicros) supersedes it for display, so the estimate is
	// skipped in that case to avoid storing a number nobody will show.
	var estimatedCostMicros *int64
	if row.providerCostMicros == nil && row.providerType != nil && row.resolvedModel != nil &&
		(row.inputTokens != nil || row.outputTokens != nil || row.cacheReadInputTokens != nil || row.cacheCreationInputTokens != nil) {
		var in, out, cr, cw int64
		if row.inputTokens != nil {
			in = *row.inputTokens
		}
		if row.outputTokens != nil {
			out = *row.outputTokens
		}
		if row.cacheReadInputTokens != nil {
			cr = *row.cacheReadInputTokens
		}
		if row.cacheCreationInputTokens != nil {
			cw = *row.cacheCreationInputTokens
		}
		if micros, ok := s.providers.Registry().EstimatedCostMicros(*row.providerType, *row.resolvedModel, in, out, cr, cw); ok {
			estimatedCostMicros = &micros
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
		EstimatedCostMicros:      estimatedCostMicros,
		ProviderCostMicros:       row.providerCostMicros,
		InputTokensEstimated:     row.inputTokensEstimated,
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

// outputObserver records the delay from an attempt's start to its first
// client-visible assistant frame. It records only a duration; it never sees or
// retains any response content. A nil observer is a valid no-op.
type outputObserver struct {
	started time.Time
	done    bool
	record  func(time.Duration)
}

// observe records the first-output latency exactly once. Safe on a nil receiver
// and safe to call from the streaming goroutine.
func (o *outputObserver) observe() {
	if o == nil || o.done {
		return
	}
	o.done = true
	if o.record != nil {
		o.record(time.Since(o.started))
	}
}

// newOutputObserver builds the first-output observer for the committed attempt.
// It stores the latency on that attempt for the Activity record and, for Codex
// targets, emits a dedicated log line so a silent reasoning prefill is
// distinguishable from a genuinely slow header response. Returns nil when there
// is no committed attempt to attribute the measurement to.
func (s *Server) newOutputObserver(start time.Time, row *logRow, candidate resolvedRoute, headerLatencyMs int64) *outputObserver {
	if start.IsZero() || row == nil {
		return nil
	}
	return &outputObserver{
		started: start,
		record: func(d time.Duration) {
			ms := d.Milliseconds()
			if len(row.attempts) > 0 {
				row.attempts[len(row.attempts)-1].firstOutputLatencyMs = ms
			}
			if s.logger != nil && candidate.Provider.Type == "codex-subscription" {
				s.logger.Info("codex first output",
					"client_request_id", row.clientRequestID,
					"provider", candidate.Provider.Name,
					"model", candidate.UpstreamModelID,
					"header_latency_ms", headerLatencyMs,
					"first_output_latency_ms", ms,
				)
			}
		},
	}
}

// usageCapture accumulates token counts extracted from a response body in
// memory. Only the numbers are ever retained; the body is discarded.
type usageCapture struct {
	inputTokens              *int64
	outputTokens             *int64
	cacheReadInputTokens     *int64 // OpenAI cached_tokens / Anthropic cache_read_input_tokens
	cacheCreationInputTokens *int64 // Anthropic cache_creation_input_tokens
	// providerCostMicros is a provider-reported exact cost in micro-dollars
	// (OpenRouter's usage.cost, a USD float). Nil when the provider did not
	// report one. It is authoritative over any models.dev estimate.
	providerCostMicros *int64
	// inputTokensEstimated reports that inputTokens is a local heuristic
	// estimate because the provider omitted usage. Set by the Slice 4 fallback.
	inputTokensEstimated bool
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
	setProviderCostFromUsage(u, usage)
}

// setProviderCostFromUsage records an exact, provider-reported request cost.
// OpenRouter reports usage.cost as a USD float; it is converted to
// micro-dollars (1e-6 USD, rounded) so the value survives the integer column
// without a float comparison at read time. First non-nil wins.
func setProviderCostFromUsage(u map[string]any, usage *usageCapture) {
	if usage.providerCostMicros != nil {
		return
	}
	cost, ok := u["cost"].(float64)
	if !ok {
		return
	}
	if cost < 0 {
		return
	}
	micros := int64(math.Round(cost * 1_000_000))
	usage.providerCostMicros = &micros
}

// captureStreamUsage extracts usage from a single SSE event payload, handling
// the shape each upstream protocol uses.
func captureStreamUsage(payload map[string]any, target providers.Protocol, usage *usageCapture) {
	switch target {
	case providers.ProtocolChat:
		if u, ok := payload["usage"].(map[string]any); ok {
			setUsage(usage, u["prompt_tokens"], u["completion_tokens"])
			setCacheFromUsage(u, usage)
			setProviderCostFromUsage(u, usage)
		}
	case providers.ProtocolMessages:
		if u, ok := payload["usage"].(map[string]any); ok {
			setUsage(usage, nil, u["output_tokens"])
			setCacheFromUsage(u, usage)
			setProviderCostFromUsage(u, usage)
		}
		if msg, ok := payload["message"].(map[string]any); ok {
			if u, ok := msg["usage"].(map[string]any); ok {
				setUsage(usage, u["input_tokens"], nil)
				setCacheFromUsage(u, usage)
				setProviderCostFromUsage(u, usage)
			}
		}
	case providers.ProtocolResponses:
		if resp, ok := payload["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				setUsage(usage, u["input_tokens"], u["output_tokens"])
				setCacheFromUsage(u, usage)
				setProviderCostFromUsage(u, usage)
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
