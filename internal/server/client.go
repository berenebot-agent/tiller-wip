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
	"net/url"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers"
)

func (s *Server) clientModels(w http.ResponseWriter, r *http.Request) {
	identity := r.Context().Value(clientKey).(auth.ClientIdentity)
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT canonical, context_length, max_output_tokens FROM (
	SELECT p.name||'/'||m.upstream_model_id canonical, m.context_length, m.max_output_tokens FROM client_model_permissions x JOIN provider_models m ON x.model_kind='real' AND x.model_id=m.id JOIN providers p ON p.id=m.provider_id WHERE x.client_key_id=? AND x.enabled=1 AND m.available=1 AND p.enabled=1
	UNION ALL
	SELECT g.name||'/'||v.name canonical, min(t.context_length), min(t.max_output_tokens) FROM client_model_permissions x JOIN virtual_models v ON x.model_kind='virtual' AND x.model_id=v.id JOIN virtual_provider_groups g ON g.id=v.virtual_group_id JOIN (SELECT x.virtual_model_id,m.context_length,m.max_output_tokens FROM virtual_model_targets x JOIN provider_models m ON m.id=x.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE x.enabled=1 AND m.available=1 AND p.enabled=1) t ON t.virtual_model_id=v.id WHERE x.client_key_id=? AND x.enabled=1 GROUP BY v.id
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
		if rows.Scan(&modelID, &contextLength, &maxOutputTokens) == nil {
			entry := map[string]any{"id": modelID, "object": "model", "created": 0, "owned_by": "tiller-router"}
			if contextLength.Valid && contextLength.Int64 > 0 {
				entry["context_length"] = contextLength.Int64
			}
			if maxOutputTokens.Valid && maxOutputTokens.Int64 > 0 {
				entry["max_output_tokens"] = maxOutputTokens.Int64
			}
			data = append(data, entry)
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

type resolvedRoute struct {
	Provider                        providers.Instance
	UpstreamModelID, RequestedModel string
	NativeProtocol                  providers.Protocol
	Virtual, Available              bool
	RoutingMode                     string
	Targets                         []resolvedRoute
}

func (s *Server) resolveRoute(ctx context.Context, clientID, requested string) (resolvedRoute, error) {
	tx, err := s.db.SQL.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return resolvedRoute{}, err
	}
	defer tx.Rollback()
	var route resolvedRoute
	// Direct real models remain single-target and never participate in fallback.
	var direct int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM client_model_permissions x JOIN provider_models m ON x.model_kind='real' AND x.model_id=m.id JOIN providers p ON p.id=m.provider_id WHERE x.client_key_id=? AND x.enabled=1 AND p.name||'/'||m.upstream_model_id=?`, clientID, requested).Scan(&direct)
	if err != nil {
		return resolvedRoute{}, err
	}
	if direct == 0 {
		var virtualID string
		err = tx.QueryRowContext(ctx, `SELECT v.id,v.routing_mode FROM client_model_permissions x JOIN virtual_models v ON x.model_kind='virtual' AND x.model_id=v.id JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE x.client_key_id=? AND x.enabled=1 AND g.name||'/'||v.name=?`, clientID, requested).Scan(&virtualID, &route.RoutingMode)
		if err != nil {
			return resolvedRoute{}, err
		}
		rows, e := tx.QueryContext(ctx, `SELECT p.id,p.name,p.type,p.base_url,coalesce(p.credential_secret,''),p.enabled,p.protocols,m.native_protocol,m.upstream_model_id,m.available FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=? AND t.enabled=1 ORDER BY t.position`, virtualID)
		if e != nil {
			return resolvedRoute{}, e
		}
		defer rows.Close()
		for rows.Next() {
			var target resolvedRoute
			var protocols string
			var enabled, available int
			var nativeProtocol sql.NullString
			if e = rows.Scan(&target.Provider.ID, &target.Provider.Name, &target.Provider.Type, &target.Provider.BaseURL, &target.Provider.Credential, &enabled, &protocols, &nativeProtocol, &target.UpstreamModelID, &available); e != nil {
				return resolvedRoute{}, e
			}
			target.Provider.Enabled = scanBool(enabled)
			target.Provider.Protocols = providers.DecodeProtocols(protocols)
			if nativeProtocol.Valid {
				target.NativeProtocol = providers.Protocol(nativeProtocol.String)
			}
			target.Available = target.Provider.Enabled && scanBool(available)
			target.Virtual = true
			target.RequestedModel = requested
			route.Targets = append(route.Targets, target)
		}
		if e = rows.Err(); e != nil {
			return resolvedRoute{}, e
		}
		route.Virtual, route.RequestedModel = true, requested
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
	err = tx.QueryRowContext(ctx, `SELECT p.id,p.name,p.type,p.base_url,coalesce(p.credential_secret,''),p.enabled,p.protocols,m.native_protocol,m.upstream_model_id,m.available FROM client_model_permissions x JOIN provider_models m ON x.model_kind='real' AND x.model_id=m.id JOIN providers p ON p.id=m.provider_id WHERE x.client_key_id=? AND x.enabled=1 AND p.name||'/'||m.upstream_model_id=?`, clientID, requested).Scan(&route.Provider.ID, &route.Provider.Name, &route.Provider.Type, &route.Provider.BaseURL, &route.Provider.Credential, &enabled, &protocols, &nativeProtocol, &route.UpstreamModelID, &modelAvailable)
	if err != nil {
		return resolvedRoute{}, err
	}
	route.Provider.Enabled = scanBool(enabled)
	route.Provider.Protocols = providers.DecodeProtocols(protocols)
	if nativeProtocol.Valid {
		route.NativeProtocol = providers.Protocol(nativeProtocol.String)
	}
	route.RequestedModel = requested
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
	var stream bool
	if json.Unmarshal(raw["stream"], &stream) == nil {
		row.streaming = stream
	}
	originalBody := append([]byte(nil), body...)
	start := time.Now()
	defer func() {
		row.latencyMs = time.Since(start).Milliseconds()
		s.writeLog(context.Background(), row)
	}()
	w.Header().Set("X-Tiller-Request-Id", row.clientRequestID)

	route, err := s.resolveRoute(r.Context(), identity.ID, requested)
	if err == sql.ErrNoRows {
		row.httpStatus = 404
		row.errorText = strPtr("model_not_found")
		inferenceError(w, 404, "invalid_request_error", "model_not_found", "Model not found.", incoming == providers.ProtocolMessages)
		return
	} else if err != nil {
		row.httpStatus = 500
		row.errorText = strPtr("database_error")
		inferenceError(w, 500, "server_error", "database_error", "Could not resolve the model.", incoming == providers.ProtocolMessages)
		return
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
	for _, candidate := range candidates {
		attemptStart := time.Now()
		if !candidate.Available {
			row.attempts = append(row.attempts, requestAttempt{provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "unavailable"})
			continue
		}
		target = compatibleProtocol(candidate.Provider.Protocols, candidate.NativeProtocol, incoming)
		if target == "" {
			protocolUnavailable = true
			row.attempts = append(row.attempts, requestAttempt{provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "protocol_unavailable"})
			continue
		}
		translated = target != incoming
		attemptBody := append([]byte(nil), originalBody...)
		if translated {
			attemptBody, err = translateRequest(attemptBody, incoming, target, candidate.UpstreamModelID)
			if err != nil {
				row.httpStatus = 400
				row.errorText = strPtr("translation_error")
				inferenceError(w, 400, "invalid_request_error", "translation_error", err.Error(), incoming == providers.ProtocolMessages)
				return
			}
		} else {
			var attemptRaw map[string]json.RawMessage
			_ = json.Unmarshal(attemptBody, &attemptRaw)
			attemptRaw["model"], _ = json.Marshal(candidate.UpstreamModelID)
			attemptBody, _ = json.Marshal(attemptRaw)
		}
		endpoint, e := providers.Endpoint(candidate.Provider, target)
		if e != nil {
			row.attempts = append(row.attempts, requestAttempt{provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", failureClass: "invalid_upstream", latencyMs: time.Since(attemptStart).Milliseconds()})
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
		providers.ApplyRequestAuth(req, candidate.Provider)
		response, e := s.providers.Registry().HTTPClient().Do(req)
		if e != nil {
			attemptCancel()
			class := "upstream_unreachable"
			if errors.Is(e, context.DeadlineExceeded) || isTimeout(e) {
				class = "upstream_timeout"
			}
			row.attempts = append(row.attempts, requestAttempt{provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", failureClass: class, latencyMs: time.Since(attemptStart).Milliseconds()})
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
				inferenceError(w, 502, "api_error", class, "The upstream provider could not complete the request.", incoming == providers.ProtocolMessages)
				return
			}
			row.fallbackUsed = true
			row.fallbackReason = strPtr(class)
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			class := fmt.Sprintf("http_%d", response.StatusCode)
			response.Body.Close()
			attemptCancel()
			row.attempts = append(row.attempts, requestAttempt{provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", httpStatus: response.StatusCode, failureClass: class, latencyMs: time.Since(attemptStart).Milliseconds()})
			if !route.Virtual || !fallbackStatus(response.StatusCode) {
				row.httpStatus = response.StatusCode
				row.errorText = strPtr("upstream_error")
				inferenceError(w, response.StatusCode, "api_error", "upstream_error", fmt.Sprintf("Upstream provider returned HTTP %d.", response.StatusCode), incoming == providers.ProtocolMessages)
				return
			}
			row.fallbackUsed = true
			row.fallbackReason = strPtr(class)
			continue
		}
		if e = preflightResponse(response); e != nil {
			response.Body.Close()
			attemptCancel()
			row.attempts = append(row.attempts, requestAttempt{provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", httpStatus: response.StatusCode, failureClass: "upstream_read_error", latencyMs: time.Since(attemptStart).Milliseconds()})
			if !route.Virtual || r.Context().Err() != nil {
				row.httpStatus = 502
				row.errorText = strPtr("upstream_read_error")
				inferenceError(w, 502, "api_error", "upstream_read_error", "The upstream provider could not complete the request.", incoming == providers.ProtocolMessages)
				return
			}
			row.fallbackUsed = true
			row.fallbackReason = strPtr("upstream_read_error")
			continue
		}
		route, resp, cancel = candidate, response, attemptCancel
		row.attempts = append(row.attempts, requestAttempt{provider: route.Provider.Name, model: route.UpstreamModelID, result: "success", httpStatus: response.StatusCode, latencyMs: time.Since(attemptStart).Milliseconds()})
		break
	}
	if resp == nil {
		if protocolUnavailable {
			row.httpStatus = 400
			row.errorText = strPtr("protocol_unavailable")
			inferenceError(w, 400, "invalid_request_error", "protocol_unavailable", "The selected model does not support this client protocol.", incoming == providers.ProtocolMessages)
			return
		}
		row.httpStatus = 503
		row.errorText = strPtr("virtual_model_unavailable")
		inferenceError(w, 503, "service_unavailable_error", "virtual_model_unavailable", "The virtual model could not be served by its configured targets.", incoming == providers.ProtocolMessages)
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
		inferenceError(w, resp.StatusCode, "api_error", "upstream_error", fmt.Sprintf("Upstream provider returned HTTP %d.", resp.StatusCode), incoming == providers.ProtocolMessages)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	idle := time.AfterFunc(5*time.Minute, cancel)
	defer idle.Stop()
	reader := &idleReader{reader: resp.Body, timer: idle}
	usage := &usageCapture{}
	if translated {
		w.WriteHeader(resp.StatusCode)
		row.httpStatus = resp.StatusCode
		if err := translateResponse(w, reader, incoming, target, route, usage); err != nil {
			s.logger.Warn("protocol translation stream ended", "protocol", incoming, "upstream_protocol", target, "error_class", fmt.Sprintf("%T", err))
		}
		row.inputTokens, row.outputTokens = usage.inputTokens, usage.outputTokens
		return
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		w.WriteHeader(resp.StatusCode)
		row.httpStatus = resp.StatusCode
		rewriteSSE(w, reader, route.UpstreamModelID, route.RequestedModel, usage)
		row.inputTokens, row.outputTokens = usage.inputTokens, usage.outputTokens
		return
	}
	// Non-streaming JSON body: read fully to extract usage, then rewrite.
	body, err = io.ReadAll(io.LimitReader(reader, 64<<20))
	if err != nil {
		row.httpStatus = 502
		row.errorText = strPtr("upstream_read_error")
		return
	}
	extractUsage(body, usage)
	row.inputTokens, row.outputTokens = usage.inputTokens, usage.outputTokens
	w.WriteHeader(resp.StatusCode)
	row.httpStatus = resp.StatusCode
	_, _ = w.Write(rewriteModelBytes(body, route.UpstreamModelID, route.RequestedModel))
}

type bufferedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r bufferedReadCloser) Close() error { return r.closer.Close() }

// preflightResponse ensures a successful upstream response has produced data
// before Tiller commits anything to the client. This preserves the no-splice
// rule while allowing a different virtual target after a pre-output failure.
func preflightResponse(resp *http.Response) error {
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		first := make([]byte, 1)
		n, err := resp.Body.Read(first)
		if n == 0 && err != nil {
			return err
		}
		resp.Body = bufferedReadCloser{Reader: io.MultiReader(bytes.NewReader(first[:n]), resp.Body), closer: resp.Body}
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
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
	return status == 401 || status == 403 || status == 404 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504
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

func sameHost(rawA, rawB string) bool {
	a, _ := url.Parse(rawA)
	b, _ := url.Parse(rawB)
	return a.Scheme == b.Scheme && a.Host == b.Host
}
