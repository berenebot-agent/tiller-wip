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
	SELECT g.name||'/'||v.name canonical, m.context_length, m.max_output_tokens FROM client_model_permissions x JOIN virtual_models v ON x.model_kind='virtual' AND x.model_id=v.id JOIN virtual_provider_groups g ON g.id=v.virtual_group_id JOIN provider_models m ON m.id=v.target_provider_model_id JOIN providers p ON p.id=v.target_provider_id WHERE x.client_key_id=? AND x.enabled=1 AND m.available=1 AND p.enabled=1
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
	Virtual, Available              bool
}

func (s *Server) resolveRoute(ctx context.Context, clientID, requested string) (resolvedRoute, error) {
	tx, err := s.db.SQL.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return resolvedRoute{}, err
	}
	defer tx.Rollback()
	var route resolvedRoute
	var protocols string
	var enabled, modelAvailable int
	var routeKind string
	err = tx.QueryRowContext(ctx, `SELECT p.id,p.name,p.type,p.base_url,coalesce(p.credential_secret,''),p.enabled,p.protocols,m.upstream_model_id,m.available,kind FROM (
	SELECT p.id provider_id,m.id model_id,'real' kind FROM client_model_permissions x JOIN provider_models m ON x.model_kind='real' AND x.model_id=m.id JOIN providers p ON p.id=m.provider_id WHERE x.client_key_id=? AND x.enabled=1 AND p.name||'/'||m.upstream_model_id=?
	UNION ALL
	SELECT v.target_provider_id provider_id,v.target_provider_model_id model_id,'virtual' kind FROM client_model_permissions x JOIN virtual_models v ON x.model_kind='virtual' AND x.model_id=v.id JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE x.client_key_id=? AND x.enabled=1 AND g.name||'/'||v.name=?
) resolved JOIN providers p ON p.id=resolved.provider_id JOIN provider_models m ON m.id=resolved.model_id LIMIT 1`, clientID, requested, clientID, requested).Scan(&route.Provider.ID, &route.Provider.Name, &route.Provider.Type, &route.Provider.BaseURL, &route.Provider.Credential, &enabled, &protocols, &route.UpstreamModelID, &modelAvailable, &routeKind)
	if err != nil {
		return resolvedRoute{}, err
	}
	route.Provider.Enabled = scanBool(enabled)
	route.Provider.Protocols = providers.DecodeProtocols(protocols)
	route.RequestedModel = requested
	route.Virtual = routeKind == "virtual"
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
	row.resolvedProvider = &route.Provider.Name
	row.resolvedModel = &route.UpstreamModelID
	if !route.Available {
		row.httpStatus = 503
		row.errorText = strPtr("model_unavailable")
		inferenceError(w, 503, "service_unavailable_error", "model_unavailable", "The resolved model target is unavailable.", incoming == providers.ProtocolMessages)
		return
	}
	target := incoming
	if !providers.Supports(route.Provider.Protocols, incoming) {
		if providers.Supports(route.Provider.Protocols, providers.ProtocolMessages) {
			target = providers.ProtocolMessages
		} else if providers.Supports(route.Provider.Protocols, providers.ProtocolChat) {
			target = providers.ProtocolChat
		} else if providers.Supports(route.Provider.Protocols, providers.ProtocolResponses) {
			target = providers.ProtocolResponses
		} else {
			row.httpStatus = 503
			row.errorText = strPtr("protocol_unavailable")
			inferenceError(w, 503, "service_unavailable_error", "protocol_unavailable", "The provider has no compatible protocol surface.", incoming == providers.ProtocolMessages)
			return
		}
	}
	translated := target != incoming
	if translated {
		body, err = translateRequest(body, incoming, target, route.UpstreamModelID)
		if err != nil {
			var unsupported unsupportedFeature
			if errors.As(err, &unsupported) {
				row.httpStatus = 400
				row.errorText = strPtr("unsupported_feature")
				inferenceError(w, 400, "invalid_request_error", "unsupported_feature", unsupported.Error(), incoming == providers.ProtocolMessages)
			} else {
				row.httpStatus = 400
				row.errorText = strPtr("translation_error")
				inferenceError(w, 400, "invalid_request_error", "translation_error", err.Error(), incoming == providers.ProtocolMessages)
			}
			return
		}
	} else {
		raw["model"], _ = json.Marshal(route.UpstreamModelID)
		body, _ = json.Marshal(raw)
	}
	endpoint, err := providers.Endpoint(route.Provider, target)
	if err != nil {
		row.httpStatus = 502
		row.errorText = strPtr("invalid_upstream")
		inferenceError(w, 502, "api_error", "invalid_upstream", "The upstream endpoint is invalid.", incoming == providers.ProtocolMessages)
		return
	}
	upstreamCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		row.httpStatus = 502
		row.errorText = strPtr("invalid_upstream")
		inferenceError(w, 502, "api_error", "invalid_upstream", "The upstream endpoint is invalid.", incoming == providers.ProtocolMessages)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "Tiller-Router/1")
	providers.ApplyRequestAuth(req, route.Provider)
	resp, err := s.providers.Registry().HTTPClient().Do(req)
	if err != nil {
		status := http.StatusBadGateway
		code := "upstream_unreachable"
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			status = http.StatusGatewayTimeout
			code = "upstream_timeout"
		}
		row.httpStatus = status
		row.errorText = strPtr(code)
		inferenceError(w, status, "api_error", code, "The upstream provider could not complete the request.", incoming == providers.ProtocolMessages)
		return
	}
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
