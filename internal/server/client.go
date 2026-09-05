package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers"
)

const maxUpstreamNonStreamBytes int64 = 64 << 20

// maxUpstreamErrorBytes bounds how much of an upstream error response body we
// read in order to pass it through verbatim to the originating client. The
// body is only ever written directly to the client — it is never stored in
// the activity log (privacy guardrail). Virtual fallback paths never read
// the body at all: they close it immediately and move on.
const maxUpstreamErrorBytes int64 = 1 << 20

var errUpstreamResponseTooLarge = errors.New("upstream response exceeds the non-streaming response limit")

func (s *Server) clientModels(w http.ResponseWriter, r *http.Request) {
	identity := r.Context().Value(clientKey).(auth.ClientIdentity)
	var keyType string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT key_type FROM client_keys WHERE id=?`, identity.ID).Scan(&keyType); err != nil {
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
		return
	}
	if keyType == "single" {
		var modelName, realID, virtualID string
		var real, virtual sql.NullString
		if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT exposed_model_name,real_model_id,virtual_model_id FROM client_single_bindings WHERE client_key_id=?`, identity.ID).Scan(&modelName, &real, &virtual); err != nil {
			inferenceError(w, 500, "server_error", "invalid_single_binding", "The Single client key is not configured correctly.", false)
			return
		}
		realID, virtualID = real.String, virtual.String
		var contextLength, maxOutputTokens sql.NullInt64
		var caps modelCapabilities
		if real.Valid {
			_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT context_length,max_output_tokens,supports_tools,supports_vision,supports_reasoning,supports_structured_output FROM provider_models WHERE id=?`, realID).Scan(&contextLength, &maxOutputTokens, &caps.Tools, &caps.Vision, &caps.Reasoning, &caps.StructuredOutput)
		} else {
			_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT `+conservativeMin("m.context_length")+`,`+conservativeMin("m.max_output_tokens")+`,`+triStateAND("m.supports_tools")+`,`+triStateAND("m.supports_vision")+`,`+triStateAND("m.supports_reasoning")+`,`+triStateAND("m.supports_structured_output")+` FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=? AND t.enabled=1 AND m.available=1 AND p.enabled=1`, virtualID).Scan(&contextLength, &maxOutputTokens, &caps.Tools, &caps.Vision, &caps.Reasoning, &caps.StructuredOutput)
		}
		entry := map[string]any{"id": modelName, "object": "model", "created": 0, "owned_by": "tiller-router"}
		if contextLength.Valid && contextLength.Int64 > 0 {
			entry["context_length"] = contextLength.Int64
		}
		if maxOutputTokens.Valid && maxOutputTokens.Int64 > 0 {
			entry["max_output_tokens"] = maxOutputTokens.Int64
		}
		caps.addTo(entry)
		writeJSON(w, 200, map[string]any{"object": "list", "data": []map[string]any{entry}})
		return
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT canonical, context_length, max_output_tokens, supports_tools, supports_vision, supports_reasoning, supports_structured_output FROM (
	SELECT p.name||'/'||m.upstream_model_id canonical, m.context_length, m.max_output_tokens, m.supports_tools, m.supports_vision, m.supports_reasoning, m.supports_structured_output FROM client_model_permissions x JOIN provider_models m ON x.model_kind='real' AND x.model_id=m.id JOIN providers p ON p.id=m.provider_id WHERE x.client_key_id=? AND x.enabled=1 AND m.available=1 AND p.enabled=1
	UNION ALL
	SELECT g.name||'/'||v.name canonical, `+conservativeMin("t.context_length")+`, `+conservativeMin("t.max_output_tokens")+`, `+triStateAND("t.supports_tools")+`, `+triStateAND("t.supports_vision")+`, `+triStateAND("t.supports_reasoning")+`, `+triStateAND("t.supports_structured_output")+` FROM client_model_permissions x JOIN virtual_models v ON x.model_kind='virtual' AND x.model_id=v.id JOIN virtual_provider_groups g ON g.id=v.virtual_group_id JOIN (SELECT x.virtual_model_id,m.context_length,m.max_output_tokens,m.supports_tools,m.supports_vision,m.supports_reasoning,m.supports_structured_output FROM virtual_model_targets x JOIN provider_models m ON m.id=x.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE x.enabled=1 AND m.available=1 AND p.enabled=1) t ON t.virtual_model_id=v.id WHERE x.client_key_id=? AND x.enabled=1 GROUP BY v.id
) ORDER BY canonical`, identity.ID, identity.ID)
	if err != nil {
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
		return
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		var modelID string
		var contextLength sql.NullInt64
		var maxOutputTokens sql.NullInt64
		var caps modelCapabilities
		if rows.Scan(&modelID, &contextLength, &maxOutputTokens, &caps.Tools, &caps.Vision, &caps.Reasoning, &caps.StructuredOutput) == nil {
			entry := map[string]any{"id": modelID, "object": "model", "created": 0, "owned_by": "tiller-router"}
			if contextLength.Valid && contextLength.Int64 > 0 {
				entry["context_length"] = contextLength.Int64
			}
			if maxOutputTokens.Valid && maxOutputTokens.Int64 > 0 {
				entry["max_output_tokens"] = maxOutputTokens.Int64
			}
			caps.addTo(entry)
			data = append(data, entry)
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// modelCapabilities holds the tri-state capability flags for a model. A flag is
// Valid only when the provider reported it; unknown flags are omitted from the
// client-facing catalogue rather than being reported as unsupported.
type modelCapabilities struct {
	Tools, Vision, Reasoning, StructuredOutput sql.NullInt64
}

func (c modelCapabilities) addTo(entry map[string]any) {
	if c.Tools.Valid {
		entry["supports_tools"] = c.Tools.Int64
	}
	if c.Vision.Valid {
		entry["supports_vision"] = c.Vision.Int64
	}
	if c.Reasoning.Valid {
		entry["supports_reasoning"] = c.Reasoning.Int64
	}
	if c.StructuredOutput.Valid {
		entry["supports_structured_output"] = c.StructuredOutput.Int64
	}
}

// triStateAND builds a SQLite expression computing the conservative AND of a
// tri-state capability column across a group: any 0 -> 0; else any NULL ->
// NULL; else 1. An empty group yields NULL (unknown).
func triStateAND(col string) string {
	return `CASE WHEN COUNT(*)=0 THEN NULL WHEN COUNT(CASE WHEN ` + col + `=0 THEN 1 END)>0 THEN 0 WHEN COUNT(CASE WHEN ` + col + ` IS NULL THEN 1 END)>0 THEN NULL ELSE 1 END`
}

// conservativeMin advertises a numeric limit only when every eligible target
// reports a positive value. A missing value must keep the aggregate unknown:
// assuming the minimum of the known subset could overstate an unreported
// target's safe limit.
func conservativeMin(col string) string {
	return `CASE WHEN COUNT(*)=0 OR COUNT(` + col + `)<>COUNT(*) OR MIN(` + col + `)<=0 THEN NULL ELSE MIN(` + col + `) END`
}

type resolvedRoute struct {
	Provider                            providers.Instance
	ProviderModelID                     string
	UpstreamModelID, RequestedModel     string
	NativeProtocol                      providers.Protocol
	Virtual, Available                  bool
	Targets                             []resolvedRoute
	RouteKind, RouteModelID, RouteModel string
}

func (s *Server) resolveRoute(ctx context.Context, clientID, requested string) (resolvedRoute, error) {
	tx, err := s.db.SQL.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return resolvedRoute{}, err
	}
	defer tx.Rollback()
	var keyType string
	if err = tx.QueryRowContext(ctx, `SELECT key_type FROM client_keys WHERE id=?`, clientID).Scan(&keyType); err != nil {
		return resolvedRoute{}, err
	}
	var route resolvedRoute
	clientModel := requested
	if keyType == "single" {
		var realID, virtualID sql.NullString
		if err = tx.QueryRowContext(ctx, `SELECT exposed_model_name,real_model_id,virtual_model_id FROM client_single_bindings WHERE client_key_id=?`, clientID).Scan(&clientModel, &realID, &virtualID); err != nil {
			return resolvedRoute{}, err
		}
		if realID.Valid {
			route.RouteKind, route.RouteModelID = "real", realID.String
			route.ProviderModelID = realID.String
			if err = tx.QueryRowContext(ctx, `SELECT p.name||'/'||m.upstream_model_id FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE m.id=?`, realID.String).Scan(&route.RouteModel); err != nil {
				return resolvedRoute{}, err
			}
		} else {
			route.RouteKind, route.RouteModelID = "virtual", virtualID.String
			if err = tx.QueryRowContext(ctx, `SELECT g.name||'/'||v.name FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE v.id=?`, virtualID.String).Scan(&route.RouteModel); err != nil {
				return resolvedRoute{}, err
			}
		}
	} else {
		err = tx.QueryRowContext(ctx, `SELECT m.id,p.name||'/'||m.upstream_model_id FROM client_model_permissions x JOIN provider_models m ON x.model_kind='real' AND x.model_id=m.id JOIN providers p ON p.id=m.provider_id WHERE x.client_key_id=? AND x.enabled=1 AND p.name||'/'||m.upstream_model_id=?`, clientID, requested).Scan(&route.RouteModelID, &route.RouteModel)
		if err == nil {
			route.RouteKind = "real"
		} else if err == sql.ErrNoRows {
			err = tx.QueryRowContext(ctx, `SELECT v.id,g.name||'/'||v.name FROM client_model_permissions x JOIN virtual_models v ON x.model_kind='virtual' AND x.model_id=v.id JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE x.client_key_id=? AND x.enabled=1 AND g.name||'/'||v.name=?`, clientID, requested).Scan(&route.RouteModelID, &route.RouteModel)
			if err != nil {
				return resolvedRoute{}, err
			}
			route.RouteKind = "virtual"
		} else {
			return resolvedRoute{}, err
		}
	}
	if route.RouteKind == "virtual" {
		rows, e := tx.QueryContext(ctx, `SELECT m.id,p.id,p.name,p.type,p.base_url,coalesce(p.credential_secret,''),p.enabled,p.protocols,m.native_protocol,m.upstream_model_id,m.available FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=? AND t.enabled=1 ORDER BY t.position`, route.RouteModelID)
		if e != nil {
			return resolvedRoute{}, e
		}
		defer rows.Close()
		for rows.Next() {
			var target resolvedRoute
			var protocols string
			var enabled, available int
			var nativeProtocol sql.NullString
			if e = rows.Scan(&target.ProviderModelID, &target.Provider.ID, &target.Provider.Name, &target.Provider.Type, &target.Provider.BaseURL, &target.Provider.Credential, &enabled, &protocols, &nativeProtocol, &target.UpstreamModelID, &available); e != nil {
				return resolvedRoute{}, e
			}
			target.Provider.Enabled = scanBool(enabled)
			target.Provider.Protocols = providers.DecodeProtocols(protocols)
			s.providers.HydrateOAuth(ctx, &target.Provider)
			if nativeProtocol.Valid {
				target.NativeProtocol = providers.Protocol(nativeProtocol.String)
			}
			target.Available = target.Provider.Enabled && scanBool(available)
			target.Virtual, target.RequestedModel = true, clientModel
			target.RouteKind, target.RouteModelID, target.RouteModel = route.RouteKind, route.RouteModelID, route.RouteModel
			route.Targets = append(route.Targets, target)
		}
		if e = rows.Err(); e != nil {
			return resolvedRoute{}, e
		}
		route.Virtual, route.RequestedModel = true, clientModel
		if len(route.Targets) > 0 {
			route.Provider, route.UpstreamModelID, route.Available = route.Targets[0].Provider, route.Targets[0].UpstreamModelID, route.Targets[0].Available
		}
		if err := tx.Commit(); err != nil {
			return resolvedRoute{}, err
		}
		return route, nil
	}
	var protocols string
	var enabled, modelAvailable int
	var nativeProtocol sql.NullString
	route.ProviderModelID = route.RouteModelID
	err = tx.QueryRowContext(ctx, `SELECT p.id,p.name,p.type,p.base_url,coalesce(p.credential_secret,''),p.enabled,p.protocols,m.native_protocol,m.upstream_model_id,m.available FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE m.id=?`, route.RouteModelID).Scan(&route.Provider.ID, &route.Provider.Name, &route.Provider.Type, &route.Provider.BaseURL, &route.Provider.Credential, &enabled, &protocols, &nativeProtocol, &route.UpstreamModelID, &modelAvailable)
	if err != nil {
		return resolvedRoute{}, err
	}
	route.Provider.Enabled = scanBool(enabled)
	route.Provider.Protocols = providers.DecodeProtocols(protocols)
	s.providers.HydrateOAuth(ctx, &route.Provider)
	if nativeProtocol.Valid {
		route.NativeProtocol = providers.Protocol(nativeProtocol.String)
	}
	route.RequestedModel = clientModel
	route.Virtual = false
	route.Available = route.Provider.Enabled && scanBool(modelAvailable)
	if err := tx.Commit(); err != nil {
		return resolvedRoute{}, err
	}
	return route, nil
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, incoming providers.Protocol) {
	identity := r.Context().Value(clientKey).(auth.ClientIdentity)
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		inferenceError(w, 400, "invalid_request_error", "request_too_large", "Request JSON exceeds the 32 MiB limit.", incoming == providers.ProtocolMessages)
		return
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		inferenceError(w, 400, "invalid_request_error", "invalid_json", "Request body must be valid JSON.", incoming == providers.ProtocolMessages)
		return
	}
	var requested string
	if json.Unmarshal(raw["model"], &requested) != nil || requested == "" {
		inferenceError(w, 400, "invalid_request_error", "model_required", "A model identifier is required.", incoming == providers.ProtocolMessages)
		return
	}
	// Begin request logging once a valid client + model is present. The row is
	// built up as the request progresses and written once, synchronously, in a
	// deferred best-effort insert that never fails the request.
	row := &logRow{
		clientKeyID:     identity.ID,
		requestedModel:  requested,
		protocol:        string(incoming),
		clientRequestID: newRequestID(),
		createdAt:       database.Now(),
	}
	logErrorBodies, _ := s.db.GetLogErrorBodies(r.Context())
	originalBody := append([]byte(nil), body...)
	start := time.Now()
	streamed := false
	clientTracked := false
	activeTargetID := ""
	var route resolvedRoute
	defer func() {
		row.latencyMs = time.Since(start).Milliseconds()
		if logErrorBodies && row.httpStatus >= 400 {
			row.requestBody, row.requestBodyTruncated = loggedBody(originalBody)
		}
		if route.Virtual {
			s.inflight.end(route.RouteModelID, streamed)
		}
		if clientTracked {
			s.inflight.clientEnd(row.clientKeyID, streamed)
		}
		if activeTargetID != "" {
			s.inflight.targetEnd(route.RouteModelID, activeTargetID)
		}
		s.writeLog(context.Background(), row)
	}()
	w.Header().Set("X-Tiller-Request-Id", row.clientRequestID)

	route, err = s.resolveRoute(r.Context(), identity.ID, requested)
	if err == sql.ErrNoRows {
		row.httpStatus = 404
		row.errorText = strPtr("model_not_found")
		inferenceError(w, 404, "invalid_request_error", "model_not_found", "Model not found.", incoming == providers.ProtocolMessages)
		return
	} else if err != nil {
		if s.logger != nil {
			s.logger.Warn("resolveRoute failed", "client_request_id", row.clientRequestID, "requested_model", requested, "error", err.Error())
		}
		row.httpStatus = 500
		row.errorText = strPtr("database_error")
		inferenceError(w, 500, "server_error", "database_error", "Could not resolve the model.", incoming == providers.ProtocolMessages)
		return
	}
	row.exposedModel = &route.RequestedModel
	row.routeKind = &route.RouteKind
	row.routeModelID = &route.RouteModelID
	row.routeModel = &route.RouteModel
	s.inflight.clientStart(row.clientKeyID)
	clientTracked = true
	if route.Virtual {
		s.inflight.start(route.RouteModelID)
	}
	candidates := []resolvedRoute{route}
	if route.Virtual {
		candidates = route.Targets
	}
	var resp *http.Response
	var target providers.Protocol
	var translated bool
	var cancel context.CancelFunc
	protocolUnavailable := false
	terminalPreflightClass := ""
	oauthRefreshed := make(map[string]bool)
	for i := 0; i < len(candidates); i++ {
		candidate := candidates[i]
		if ctxErr := r.Context().Err(); ctxErr != nil {
			class := "client_cancelled"
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				class = "client_timeout"
			}
			row.httpStatus = 502
			row.errorText = strPtr(class)
			row.fallbackReason = strPtr(class)
			inferenceError(w, 502, "api_error", class, "The client request ended before fallback could complete.", incoming == providers.ProtocolMessages)
			return
		}
		attemptStart := time.Now()
		if !candidate.Available {
			row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "unavailable"})
			continue
		}
		target = compatibleProtocol(candidate.Provider.Protocols, candidate.NativeProtocol, incoming)
		if target == "" {
			protocolUnavailable = true
			row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "protocol_unavailable"})
			continue
		}
		translated = target != incoming
		attemptBody := append([]byte(nil), originalBody...)
		if translated {
			attemptBody, err = translateRequest(attemptBody, incoming, target, candidate.UpstreamModelID)
			if err != nil {
				code := "translation_error"
				var unsupported unsupportedFeature
				if errors.As(err, &unsupported) {
					code = "unsupported_feature"
				}
				row.httpStatus = 400
				row.errorText = strPtr(code)
				inferenceError(w, 400, "invalid_request_error", code, err.Error(), incoming == providers.ProtocolMessages)
				return
			}
		} else {
			var attemptRaw map[string]json.RawMessage
			_ = json.Unmarshal(attemptBody, &attemptRaw)
			attemptRaw["model"], _ = json.Marshal(candidate.UpstreamModelID)
			attemptBody, _ = json.Marshal(attemptRaw)
		}
		if candidate.Provider.Type == "codex-subscription" {
			attemptBody, err = normalizeCodexRequest(attemptBody)
			if err != nil {
				row.httpStatus = 400
				row.errorText = strPtr("invalid_request")
				inferenceError(w, 400, "invalid_request_error", "invalid_request", "The Codex request could not be normalized.", incoming == providers.ProtocolMessages)
				return
			}
		}
		endpoint, e := providers.Endpoint(candidate.Provider, target)
		if e != nil {
			row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", failureClass: "invalid_upstream", latencyMs: time.Since(attemptStart).Milliseconds()})
			continue
		}
		upstreamCtx, attemptCancel := context.WithCancel(r.Context())
		req, e := http.NewRequestWithContext(upstreamCtx, http.MethodPost, endpoint, bytes.NewReader(attemptBody))
		if e != nil {
			attemptCancel()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("User-Agent", "Tiller-Router/1")
		if candidate.Provider.Type == "opencode-free" {
			if clientIP := s.requestClientIP(r); clientIP != "" {
				req.Header.Set("X-Real-IP", clientIP)
			}
		}
		providers.ApplyRequestAuth(req, candidate.Provider)
		if candidate.Provider.Type == "codex-subscription" {
			req.Header.Set("session-id", row.clientRequestID)
		}
		targetID := candidate.ProviderModelID
		if targetID == "" {
			targetID = candidate.Provider.Name + "/" + candidate.UpstreamModelID
		}
		if route.Virtual {
			s.inflight.targetStart(route.RouteModelID, targetID)
		}
		response, e := s.providers.Registry().HTTPClient().Do(req)
		if e != nil {
			if route.Virtual {
				s.inflight.targetEnd(route.RouteModelID, targetID)
			}
			attemptCancel()
			class := "upstream_unreachable"
			if errors.Is(e, context.DeadlineExceeded) || isTimeout(e) {
				class = "upstream_timeout"
			}
			row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", failureClass: class, errorMessage: strPtrIfNonEmpty(fixedUpstreamErrorMessage(class)), latencyMs: time.Since(attemptStart).Milliseconds()})
			if r.Context().Err() != nil {
				if errors.Is(r.Context().Err(), context.DeadlineExceeded) {
					class = "client_timeout"
				} else {
					class = "client_cancelled"
				}
				row.httpStatus = 502
				row.errorText = strPtr(class)
				row.fallbackReason = strPtr(class)
				inferenceError(w, 502, "api_error", class, "The client request ended before fallback could complete.", incoming == providers.ProtocolMessages)
				return
			}
			if !route.Virtual {
				row.httpStatus = 502
				row.errorText = strPtr(class)
				row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
				inferenceError(w, 502, "api_error", class, "The upstream provider could not complete the request.", incoming == providers.ProtocolMessages)
				return
			}
			row.fallbackUsed = true
			row.fallbackReason = strPtr(class)
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			if route.Virtual {
				s.inflight.targetEnd(route.RouteModelID, targetID)
			}
			class := fmt.Sprintf("http_%d", response.StatusCode)
			var upstreamErrorBody []byte
			var upstreamErrorReadErr error
			if !route.Virtual || logErrorBodies {
				// Read the upstream error body for bounded passthrough to the
				// originating client. When sensitive body logging is enabled,
				// retain the bounded body on the failed attempt as well.
				upstreamErrorBody, upstreamErrorReadErr = io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBytes+1))
			}
			response.Body.Close()
			attemptCancel()
			attempt := requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", httpStatus: response.StatusCode, failureClass: class, latencyMs: time.Since(attemptStart).Milliseconds()}
			if logErrorBodies && upstreamErrorReadErr == nil && len(upstreamErrorBody) > 0 {
				attempt.errorBody, attempt.errorBodyTruncated = loggedBody(upstreamErrorBody)
			}
			row.attempts = append(row.attempts, attempt)
			// Stale-auth recovery: on 401/403 from an OAuth provider, force a
			// token refresh once per request and retry the same target before
			// falling through to normal virtual fallback. ForceOAuthRefresh
			// transitions auth_state on failure, so a dead refresh token surfaces
			// as reconnect_required without further handling here.
			if !oauthRefreshed[candidate.Provider.ID] && (response.StatusCode == 401 || response.StatusCode == 403) {
				if descriptor, ok := providers.Lookup(candidate.Provider.Type); ok && descriptor.AuthMode == providers.AuthModeOAuth {
					if refreshErr := s.providers.ForceOAuthRefresh(r.Context(), &candidate.Provider); refreshErr == nil {
						oauthRefreshed[candidate.Provider.ID] = true
						candidates[i].Provider = candidate.Provider
						i--
						continue
					}
				}
			}
			// An upstream HTTP response is an upstream failure regardless of
			// status. Ordered virtual routes try their next target by default;
			// router-side failures (for example translation errors) are handled
			// before this point and must not be hidden by fallback.
			if !route.Virtual || !fallbackStatus(response.StatusCode) {
				row.httpStatus = response.StatusCode
				row.errorText = strPtr("upstream_error")
				if logErrorBodies && upstreamErrorReadErr == nil && len(upstreamErrorBody) > 0 {
					row.errorBody, row.errorBodyTruncated = loggedBody(upstreamErrorBody)
				}
				// Direct (non-virtual, non-translated) routes pass through
				// the provider's structured error body verbatim so the
				// client sees the provider's error shape. The body is
				// bounded and never persisted.
				if upstreamErrorReadErr == nil && !translated && len(upstreamErrorBody) > 0 && int64(len(upstreamErrorBody)) <= maxUpstreamErrorBytes {
					copySafeResponseHeaders(w.Header(), response.Header)
					upstreamErrorBody = rewriteModelBytes(upstreamErrorBody, route.UpstreamModelID, route.RequestedModel)
					if route.UpstreamModelID != route.RequestedModel {
						upstreamErrorBody = bytes.ReplaceAll(upstreamErrorBody, []byte(route.UpstreamModelID), []byte(route.RequestedModel))
					}
					w.WriteHeader(response.StatusCode)
					_, _ = w.Write(upstreamErrorBody)
					return
				}
				inferenceError(w, response.StatusCode, "api_error", "upstream_error", fmt.Sprintf("Upstream provider returned HTTP %d.", response.StatusCode), incoming == providers.ProtocolMessages)
				return
			}
			row.fallbackUsed = true
			row.fallbackReason = strPtr(class)
			continue
		}
		if e = preflightResponseLimit(response, maxUpstreamNonStreamBytes); e != nil {
			if route.Virtual {
				s.inflight.targetEnd(route.RouteModelID, targetID)
			}
			response.Body.Close()
			attemptCancel()
			class := "upstream_read_error"
			message := "The upstream provider could not complete the request."
			if errors.Is(e, errUpstreamResponseTooLarge) {
				class = "upstream_response_too_large"
				message = "The upstream provider response exceeded Tiller's non-streaming response limit."
			}
			terminalPreflightClass = class
			row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", httpStatus: response.StatusCode, failureClass: class, latencyMs: time.Since(attemptStart).Milliseconds()})
			row.attempts[len(row.attempts)-1].errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
			if !route.Virtual || r.Context().Err() != nil {
				row.httpStatus = 502
				row.errorText = strPtr(class)
				row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
				inferenceError(w, 502, "api_error", class, message, incoming == providers.ProtocolMessages)
				return
			}
			row.fallbackUsed = true
			row.fallbackReason = strPtr(class)
			continue
		}
		route, resp, cancel = candidate, response, attemptCancel
		if route.Virtual {
			activeTargetID = targetID
		}
		row.attempts = append(row.attempts, requestAttempt{providerModelID: route.ProviderModelID, provider: route.Provider.Name, model: route.UpstreamModelID, result: "success", httpStatus: response.StatusCode, latencyMs: time.Since(attemptStart).Milliseconds()})
		break
	}
	// Emit a single logical notification for the routing outcome (fallback or
	// all-targets-failed). This is best-effort and never blocks or alters the
	// client response.
	s.maybeNotify(row, route, resp)
	if resp == nil {
		if terminalPreflightClass == "upstream_response_too_large" {
			row.httpStatus = 502
			row.errorText = strPtr(terminalPreflightClass)
			inferenceError(w, 502, "api_error", terminalPreflightClass, "The upstream provider response exceeded Tiller's non-streaming response limit.", incoming == providers.ProtocolMessages)
			return
		}
		if protocolUnavailable {
			row.httpStatus = 400
			row.errorText = strPtr("protocol_unavailable")
			inferenceError(w, 400, "invalid_request_error", "protocol_unavailable", "The selected model does not support this client protocol.", incoming == providers.ProtocolMessages)
			return
		}
		row.httpStatus = 503
		for i := len(row.attempts) - 1; i >= 0; i-- {
			if row.attempts[i].result == "failed" && row.attempts[i].errorMessage != nil {
				row.errorMessage = row.attempts[i].errorMessage
				break
			}
		}
		if route.Virtual {
			row.errorText = strPtr("virtual_model_unavailable")
			inferenceError(w, 503, "service_unavailable_error", "virtual_model_unavailable", "The virtual model could not be served by its configured targets.", incoming == providers.ProtocolMessages)
		} else {
			row.errorText = strPtr("model_unavailable")
			inferenceError(w, 503, "service_unavailable_error", "model_unavailable", "The configured model is unavailable.", incoming == providers.ProtocolMessages)
		}
		return
	}
	defer cancel()
	row.resolvedProvider = &route.Provider.Name
	row.resolvedModel = &route.UpstreamModelID
	defer resp.Body.Close()
	copySafeResponseHeaders(w.Header(), resp.Header)
	if v := resp.Header.Get("Request-Id"); v != "" {
		row.providerRequestID = &v
	} else if v := resp.Header.Get("X-Request-Id"); v != "" {
		row.providerRequestID = &v
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		row.httpStatus = resp.StatusCode
		row.errorText = strPtr(fmt.Sprintf("Upstream provider returned HTTP %d.", resp.StatusCode))
		row.errorMessage = strPtr(fmt.Sprintf("Upstream provider returned HTTP %d.", resp.StatusCode))
		inferenceError(w, resp.StatusCode, "api_error", "upstream_error", fmt.Sprintf("Upstream provider returned HTTP %d.", resp.StatusCode), incoming == providers.ProtocolMessages)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	idle := time.AfterFunc(5*time.Minute, cancel)
	defer idle.Stop()
	reader := &idleReader{reader: resp.Body, timer: idle}
	usage := &usageCapture{}
	if translated {
		if isStreamingResponse(resp) {
			streamed = true
			row.streaming = true
			if route.Virtual {
				s.inflight.streaming(route.RouteModelID)
			}
			s.inflight.clientStreaming(row.clientKeyID)
		}
		w.WriteHeader(resp.StatusCode)
		row.httpStatus = resp.StatusCode
		if err := translateResponse(w, reader, incoming, target, route, usage); err != nil {
			s.logger.Warn("protocol translation stream ended", "protocol", incoming, "upstream_protocol", target, "error_class", fmt.Sprintf("%T", err))
		}
		row.inputTokens, row.outputTokens = usage.inputTokens, usage.outputTokens
		row.cacheReadInputTokens, row.cacheCreationInputTokens = usage.cacheReadInputTokens, usage.cacheCreationInputTokens
		return
	}
	if isStreamingResponse(resp) {
		streamed = true
		row.streaming = true
		if route.Virtual {
			s.inflight.streaming(route.RouteModelID)
		}
		s.inflight.clientStreaming(row.clientKeyID)
		w.WriteHeader(resp.StatusCode)
		row.httpStatus = resp.StatusCode
		rewriteSSE(w, reader, route.UpstreamModelID, route.RequestedModel, usage)
		row.inputTokens, row.outputTokens = usage.inputTokens, usage.outputTokens
		row.cacheReadInputTokens, row.cacheCreationInputTokens = usage.cacheReadInputTokens, usage.cacheCreationInputTokens
		return
	}
	// Non-streaming JSON body: read fully to extract usage, then rewrite.
	body, err = io.ReadAll(reader)
	if err != nil {
		row.httpStatus = 502
		row.errorText = strPtr("upstream_read_error")
		row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("upstream_read_error"))
		return
	}
	extractUsage(body, usage)
	row.inputTokens, row.outputTokens = usage.inputTokens, usage.outputTokens
	row.cacheReadInputTokens, row.cacheCreationInputTokens = usage.cacheReadInputTokens, usage.cacheCreationInputTokens
	w.WriteHeader(resp.StatusCode)
	row.httpStatus = resp.StatusCode
	_, _ = w.Write(rewriteModelBytes(body, route.UpstreamModelID, route.RequestedModel))
}

type bufferedReadCloser struct {
	io.Reader
	closer io.Closer
}

func isStreamingResponse(resp *http.Response) bool {
	return strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
}

func (r bufferedReadCloser) Close() error { return r.closer.Close() }

// preflightResponseLimit ensures a successful upstream response has produced
// data before Tiller commits anything to the client. This preserves the
// no-splice rule while allowing a different virtual target after a pre-output
// failure.
func preflightResponseLimit(resp *http.Response, limit int64) error {
	if isStreamingResponse(resp) {
		first := make([]byte, 1)
		n, err := resp.Body.Read(first)
		if n == 0 && err != nil {
			return err
		}
		resp.Body = bufferedReadCloser{Reader: io.MultiReader(bytes.NewReader(first[:n]), resp.Body), closer: resp.Body}
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return errUpstreamResponseTooLarge
	}
	resp.Body = bufferedReadCloser{Reader: bytes.NewReader(body), closer: resp.Body}
	return nil
}

type idleReader struct {
	reader io.Reader
	timer  *time.Timer
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.reader.Read(p)
	if n > 0 {
		i.timer.Reset(5 * time.Minute)
	}
	return n, err
}

func copySafeResponseHeaders(dst, src http.Header) {
	for _, name := range []string{"Content-Type", "Cache-Control", "Retry-After", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Request-Id", "X-Request-Id"} {
		if value := src.Values(name); len(value) > 0 {
			dst.Del(name)
			for _, v := range value {
				dst.Add(name, v)
			}
		}
	}
}
func isTimeout(err error) bool {
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}

func fallbackStatus(status int) bool {
	return status < 200 || status >= 300
}

func compatibleProtocol(protocols []providers.Protocol, native providers.Protocol, incoming providers.Protocol) providers.Protocol {
	if native != "" {
		return native
	}
	if providers.Supports(protocols, incoming) {
		return incoming
	}
	for _, candidate := range []providers.Protocol{providers.ProtocolMessages, providers.ProtocolChat, providers.ProtocolResponses} {
		if providers.Supports(protocols, candidate) {
			return candidate
		}
	}
	return ""
}

func rewriteSSE(w http.ResponseWriter, r io.Reader, upstream, requested string, usage *usageCapture) {
	reader := bufio.NewReader(r)
	flusher, _ := w.(http.Flusher)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			done := false
			trim := bytes.TrimSpace(line)
			if bytes.HasPrefix(trim, []byte("data:")) {
				payload := bytes.TrimSpace(bytes.TrimPrefix(trim, []byte("data:")))
				if bytes.Equal(payload, []byte("[DONE]")) {
					done = true
				} else {
					var value any
					if json.Unmarshal(payload, &value) == nil {
						if m, ok := value.(map[string]any); ok {
							if u, ok := m["usage"].(map[string]any); ok {
								setUsage(usage, u["prompt_tokens"], u["completion_tokens"])
								setCacheFromUsage(u, usage)
							}
						}
						rewriteModel(value, upstream, requested)
						if encoded, e := json.Marshal(value); e == nil {
							prefix := line[:bytes.Index(line, []byte("data:"))]
							line = append(append(append(prefix, []byte("data: ")...), encoded...), '\n')
						}
					}
				}
			}
			_, _ = w.Write(line)
			if flusher != nil {
				flusher.Flush()
			}
			if done {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func rewriteModel(value any, upstream, requested string) {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "model" {
				if model, ok := item.(string); ok && model == upstream {
					v[key] = requested
				}
			} else {
				rewriteModel(item, upstream, requested)
			}
		}
	case []any:
		for _, item := range v {
			rewriteModel(item, upstream, requested)
		}
	}
}
