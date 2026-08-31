package server

import (
	"database/sql"
	"net/http"
)

type requestAttemptView struct {
	AttemptNumber int     `json:"attempt_number"`
	Provider      string  `json:"provider"`
	Model         string  `json:"model"`
	Result        string  `json:"result"`
	HTTPStatus    *int    `json:"http_status"`
	FailureClass  *string `json:"failure_class"`
	LatencyMs     int64   `json:"latency_ms"`
	CreatedAt     string  `json:"created_at"`
}

func (s *Server) listRequestAttempts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT attempt_number,provider,model,result,http_status,failure_class,latency_ms,created_at FROM request_attempts WHERE request_log_id=? ORDER BY attempt_number`, r.PathValue("id"))
	if err != nil {
		adminError(w, 500, "database_error", "Could not load request attempts.")
		return
	}
	defer rows.Close()
	data := []requestAttemptView{}
	for rows.Next() {
		var item requestAttemptView
		if err := rows.Scan(&item.AttemptNumber, &item.Provider, &item.Model, &item.Result, &item.HTTPStatus, &item.FailureClass, &item.LatencyMs, &item.CreatedAt); err != nil {
			adminError(w, 500, "database_error", "Could not load request attempts.")
			return
		}
		data = append(data, item)
	}
	if err := rows.Err(); err != nil {
		adminError(w, 500, "database_error", "Could not load request attempts.")
		return
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

type activityView struct {
	ID                   string  `json:"id"`
	RequestedModel       string  `json:"requested_model"`
	ExposedModel         *string `json:"exposed_model"`
	RouteKind            *string `json:"route_kind"`
	RouteModelID         *string `json:"route_model_id"`
	RouteModel           *string `json:"route_model"`
	ResolvedProvider     *string `json:"resolved_provider"`
	ResolvedModel        *string `json:"resolved_model"`
	Protocol             string  `json:"protocol"`
	Streaming            bool    `json:"streaming"`
	HTTPStatus           int     `json:"http_status"`
	LatencyMs            int64   `json:"latency_ms"`
	InputTokens          *int64  `json:"input_tokens"`
	OutputTokens         *int64  `json:"output_tokens"`
	CacheReadInputTokens *int64  `json:"cache_read_input_tokens"`
	ProviderRequestID    *string `json:"provider_request_id"`
	ClientRequestID      string  `json:"client_request_id"`
	ErrorText            *string `json:"error_text"`
	AttemptCount         int     `json:"attempt_count"`
	FallbackUsed         bool    `json:"fallback_used"`
	FallbackReason       *string `json:"fallback_reason"`
	CreatedAt            string  `json:"created_at"`
}

func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	limit, offset, search := pagination(r)
	pattern := "%" + search + "%"
	var exists int
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT count(*) FROM client_keys WHERE id=?`, clientID).Scan(&exists); err != nil || exists == 0 {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT id,requested_model,exposed_model,route_kind,route_model_id,route_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,cache_read_input_tokens,provider_request_id,client_request_id,error_text,attempt_count,fallback_used,fallback_reason,created_at FROM request_logs WHERE client_key_id=? AND (requested_model LIKE ? OR coalesce(exposed_model,'') LIKE ? OR coalesce(route_model,'') LIKE ? OR coalesce(resolved_provider,'') LIKE ? OR CAST(http_status AS TEXT) LIKE ? OR coalesce(error_text,'') LIKE ?) ORDER BY created_at DESC LIMIT ? OFFSET ?`, clientID, pattern, pattern, pattern, pattern, pattern, pattern, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	defer rows.Close()
	data := []activityView{}
	for rows.Next() {
		var v activityView
		var streaming, fallback int
		if err := rows.Scan(&v.ID, &v.RequestedModel, &v.ExposedModel, &v.RouteKind, &v.RouteModelID, &v.RouteModel, &v.ResolvedProvider, &v.ResolvedModel, &v.Protocol, &streaming, &v.HTTPStatus, &v.LatencyMs, &v.InputTokens, &v.OutputTokens, &v.CacheReadInputTokens, &v.ProviderRequestID, &v.ClientRequestID, &v.ErrorText, &v.AttemptCount, &fallback, &v.FallbackReason, &v.CreatedAt); err != nil {
			adminError(w, 500, "database_error", "Could not load activity.")
			return
		}
		v.Streaming, v.FallbackUsed = scanBool(streaming), scanBool(fallback)
		data = append(data, v)
	}
	// Guard: rows.Next() can terminate early on a row-iteration error without
	// surfacing it via Scan. Check rows.Err() so a partial result set is never
	// returned as a 200 "success". (Not unit-tested: forcing an iteration
	// failure would require weakening production code.)
	if err := rows.Err(); err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

// globalActivityView extends activityView with the client identity so the
// workspace-free Global Activity endpoint can report which client key each
// request belongs to. It reuses the activityView field definitions rather than
// duplicating incompatible row-scanning logic.
type globalActivityView struct {
	activityView
	ClientKeyID string `json:"client_key_id"`
	ClientName  string `json:"client_name"`
}

// listGlobalActivity returns recent request metadata across all client keys,
// newest first, with a deterministic id secondary sort. It is read-only and
// returns metadata only (never body-related fields).
func (s *Server) listGlobalActivity(w http.ResponseWriter, r *http.Request) {
	limit, offset, search := pagination(r)
	pattern := "%" + search + "%"
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT rl.id,rl.requested_model,rl.exposed_model,rl.route_kind,rl.route_model_id,rl.route_model,rl.resolved_provider,rl.resolved_model,rl.protocol,rl.streaming,rl.http_status,rl.latency_ms,rl.input_tokens,rl.output_tokens,rl.cache_read_input_tokens,rl.provider_request_id,rl.client_request_id,rl.error_text,rl.attempt_count,rl.fallback_used,rl.fallback_reason,rl.created_at,rl.client_key_id,ck.name FROM request_logs rl JOIN client_keys ck ON ck.id=rl.client_key_id WHERE (ck.name LIKE ? OR rl.requested_model LIKE ? OR coalesce(rl.exposed_model,'') LIKE ? OR coalesce(rl.route_model,'') LIKE ? OR coalesce(rl.resolved_provider,'') LIKE ? OR coalesce(rl.resolved_model,'') LIKE ? OR CAST(rl.http_status AS TEXT) LIKE ? OR rl.client_request_id LIKE ? OR coalesce(rl.provider_request_id,'') LIKE ? OR coalesce(rl.error_text,'') LIKE ?) ORDER BY rl.created_at DESC, rl.id DESC LIMIT ? OFFSET ?`, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	defer rows.Close()
	data := []globalActivityView{}
	for rows.Next() {
		var v globalActivityView
		var streaming, fallback int
		if err := rows.Scan(&v.ID, &v.RequestedModel, &v.ExposedModel, &v.RouteKind, &v.RouteModelID, &v.RouteModel, &v.ResolvedProvider, &v.ResolvedModel, &v.Protocol, &streaming, &v.HTTPStatus, &v.LatencyMs, &v.InputTokens, &v.OutputTokens, &v.CacheReadInputTokens, &v.ProviderRequestID, &v.ClientRequestID, &v.ErrorText, &v.AttemptCount, &fallback, &v.FallbackReason, &v.CreatedAt, &v.ClientKeyID, &v.ClientName); err != nil {
			adminError(w, 500, "database_error", "Could not load activity.")
			return
		}
		v.Streaming, v.FallbackUsed = scanBool(streaming), scanBool(fallback)
		data = append(data, v)
	}
	// Guard: rows.Next() can terminate early on a row-iteration error without
	// surfacing it via Scan. Check rows.Err() so a partial result set is never
	// returned as a 200 "success". (Not unit-tested: forcing an iteration
	// failure would require weakening production code.)
	if err := rows.Err(); err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) clearActivity(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	var exists int
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT count(*) FROM client_keys WHERE id=?`, clientID).Scan(&exists); err != nil || exists == 0 {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	if _, err := s.db.SQL.ExecContext(r.Context(), `DELETE FROM request_logs WHERE client_key_id=?`, clientID); err != nil {
		adminError(w, 500, "database_error", "Could not clear activity.")
		return
	}
	w.WriteHeader(204)
}

var _ = sql.ErrNoRows
