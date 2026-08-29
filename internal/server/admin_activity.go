package server

import (
	"database/sql"
	"net/http"
)

type activityView struct {
	ID                string  `json:"id"`
	RequestedModel    string  `json:"requested_model"`
	ResolvedProvider  *string `json:"resolved_provider"`
	ResolvedModel     *string `json:"resolved_model"`
	Protocol          string  `json:"protocol"`
	Streaming         bool    `json:"streaming"`
	HTTPStatus        int     `json:"http_status"`
	LatencyMs         int64   `json:"latency_ms"`
	InputTokens       *int64  `json:"input_tokens"`
	OutputTokens      *int64  `json:"output_tokens"`
	ProviderRequestID *string `json:"provider_request_id"`
	ClientRequestID   string  `json:"client_request_id"`
	ErrorText         *string `json:"error_text"`
	CreatedAt         string  `json:"created_at"`
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
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,provider_request_id,client_request_id,error_text,created_at FROM request_logs WHERE client_key_id=? AND (requested_model LIKE ? OR coalesce(resolved_provider,'') LIKE ? OR CAST(http_status AS TEXT) LIKE ? OR coalesce(error_text,'') LIKE ?) ORDER BY created_at DESC LIMIT ? OFFSET ?`, clientID, pattern, pattern, pattern, pattern, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	defer rows.Close()
	data := []activityView{}
	for rows.Next() {
		var v activityView
		var streaming int
		if err := rows.Scan(&v.ID, &v.RequestedModel, &v.ResolvedProvider, &v.ResolvedModel, &v.Protocol, &streaming, &v.HTTPStatus, &v.LatencyMs, &v.InputTokens, &v.OutputTokens, &v.ProviderRequestID, &v.ClientRequestID, &v.ErrorText, &v.CreatedAt); err != nil {
			adminError(w, 500, "database_error", "Could not load activity.")
			return
		}
		v.Streaming = scanBool(streaming)
		data = append(data, v)
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
