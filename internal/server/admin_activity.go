package server

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/store"
)

type requestAttemptView struct {
	AttemptNumber      int     `json:"attempt_number"`
	Provider           string  `json:"provider"`
	Model              string  `json:"model"`
	Result             string  `json:"result"`
	HTTPStatus         *int    `json:"http_status"`
	FailureClass       *string `json:"failure_class"`
	ErrorMessage       *string `json:"error_message"`
	ErrorBody          *string `json:"error_body"`
	ErrorBodyTruncated bool    `json:"error_body_truncated"`
	LatencyMs          int64   `json:"latency_ms"`
	CreatedAt          string  `json:"created_at"`
}

func (s *Server) listRequestAttempts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.scope(r).ListRequestAttempts(r.Context(), r.PathValue("id"))
	if err != nil {
		adminError(w, 500, "database_error", "Could not load request attempts.")
		return
	}
	data := make([]requestAttemptView, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		data = append(data, requestAttemptView{AttemptNumber: row.AttemptNumber, Provider: row.Provider, Model: row.Model, Result: row.Result, HTTPStatus: row.HTTPStatus, FailureClass: row.FailureClass, ErrorMessage: row.ErrorMessage, ErrorBody: row.ErrorBody, ErrorBodyTruncated: row.ErrorBodyTruncated, LatencyMs: row.LatencyMs, CreatedAt: row.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

type activityView struct {
	ID                       string  `json:"id"`
	RequestedModel           string  `json:"requested_model"`
	ExposedModel             *string `json:"exposed_model"`
	RouteKind                *string `json:"route_kind"`
	RouteModelID             *string `json:"route_model_id"`
	RouteModel               *string `json:"route_model"`
	ResolvedProvider         *string `json:"resolved_provider"`
	ResolvedModel            *string `json:"resolved_model"`
	Protocol                 string  `json:"protocol"`
	Streaming                bool    `json:"streaming"`
	HTTPStatus               int     `json:"http_status"`
	LatencyMs                int64   `json:"latency_ms"`
	InputTokens              *int64  `json:"input_tokens"`
	OutputTokens             *int64  `json:"output_tokens"`
	CacheReadInputTokens     *int64  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64  `json:"cache_creation_input_tokens"`
	ProviderRequestID        *string `json:"provider_request_id"`
	ClientRequestID          string  `json:"client_request_id"`
	ErrorText                *string `json:"error_text"`
	ErrorMessage             *string `json:"error_message"`
	RequestBody              *string `json:"request_body"`
	RequestBodyTruncated     bool    `json:"request_body_truncated"`
	ErrorBody                *string `json:"error_body"`
	ErrorBodyTruncated       bool    `json:"error_body_truncated"`
	AttemptCount             int     `json:"attempt_count"`
	AttemptRows              int     `json:"attempt_rows"`
	FallbackUsed             bool    `json:"fallback_used"`
	FallbackReason           *string `json:"fallback_reason"`
	CreatedAt                string  `json:"created_at"`
}

func activityViewFromStore(row store.ActivityRow) activityView {
	return activityView{
		ID:                       row.ID,
		RequestedModel:           row.RequestedModel,
		ExposedModel:             row.ExposedModel,
		RouteKind:                row.RouteKind,
		RouteModelID:             row.RouteModelID,
		RouteModel:               row.RouteModel,
		ResolvedProvider:         row.ResolvedProvider,
		ResolvedModel:            row.ResolvedModel,
		Protocol:                 row.Protocol,
		Streaming:                row.Streaming,
		HTTPStatus:               row.HTTPStatus,
		LatencyMs:                row.LatencyMs,
		InputTokens:              row.InputTokens,
		OutputTokens:             row.OutputTokens,
		CacheReadInputTokens:     row.CacheReadInputTokens,
		CacheCreationInputTokens: row.CacheCreationInputTokens,
		ProviderRequestID:        row.ProviderRequestID,
		ClientRequestID:          row.ClientRequestID,
		ErrorText:                row.ErrorText,
		ErrorMessage:             row.ErrorMessage,
		RequestBody:              row.RequestBody,
		RequestBodyTruncated:     row.RequestBodyTruncated,
		ErrorBody:                row.ErrorBody,
		ErrorBodyTruncated:       row.ErrorBodyTruncated,
		AttemptCount:             row.AttemptCount,
		AttemptRows:              row.AttemptRows,
		FallbackUsed:             row.FallbackUsed,
		FallbackReason:           row.FallbackReason,
		CreatedAt:                row.CreatedAt,
	}
}

func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	_ = s.flushActivity(r.Context())
	clientID := r.PathValue("id")
	limit, offset, search := pagination(r)
	sc := s.scope(r)
	exists, err := sc.ClientKeyExists(r.Context(), clientID)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	if !exists {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	rows, err := sc.ListClientActivity(r.Context(), clientID, search, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	data := make([]activityView, 0, len(rows))
	for i := range rows {
		data = append(data, activityViewFromStore(rows[i]))
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

// globalActivityView extends activityView with the client identity so the
// workspace-free Global Activity endpoint can report which client key each
// request belongs to.
type globalActivityView struct {
	activityView
	ClientKeyID string `json:"client_key_id"`
	ClientName  string `json:"client_name"`
}

// listGlobalActivity returns recent request metadata across all client keys,
// newest first, with a deterministic id secondary sort.
func (s *Server) listGlobalActivity(w http.ResponseWriter, r *http.Request) {
	_ = s.flushActivity(r.Context())
	limit, offset, search := pagination(r)
	rows, err := s.scope(r).ListGlobalActivity(r.Context(), search, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	data := make([]globalActivityView, 0, len(rows))
	for i := range rows {
		data = append(data, globalActivityView{activityView: activityViewFromStore(rows[i]), ClientKeyID: rows[i].ClientKeyID, ClientName: rows[i].ClientName})
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) clearActivity(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	sc := s.scope(r)
	exists, err := sc.ClientKeyExists(r.Context(), clientID)
	if err != nil {
		adminError(w, 500, "database_error", "Could not clear activity.")
		return
	}
	if !exists {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	if err := sc.ClearClientActivity(r.Context(), clientID); err != nil {
		adminError(w, 500, "database_error", "Could not clear activity.")
		return
	}
	s.invalidateUsageAggregates(sc.AccountID())
	w.WriteHeader(204)
}

// exportClientActivityCSV streams a CSV of the client key's activity, honouring
// the active search filter. One inference request = one row.
func (s *Server) exportClientActivityCSV(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	sc := s.scope(r)
	name, err := sc.ClientKeyName(r.Context(), clientID)
	if err != nil {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	cutoff := periodCutoffString(r.URL.Query().Get("period"))
	if err := s.writeActivityCSV(w, r, "tiller-"+sanitizeFilename(name)+"-activity-"+time.Now().UTC().Format("2006-01-02")+".csv", func(fn func(store.ActivityRow) error) error {
		return sc.ExportClientActivity(r.Context(), clientID, search, cutoff, fn)
	}); err != nil && s.logger != nil {
		s.logger.Warn("activity export failed", "error_class", fmt.Sprintf("%T", err))
	}
}

// exportVirtualActivityCSV streams a CSV of activity attributable to a virtual
// model, honouring the active search filter.
func (s *Server) exportVirtualActivityCSV(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("id")
	sc := s.scope(r)
	canonical, err := sc.VirtualModelCanonical(r.Context(), modelID)
	if err != nil {
		adminError(w, 404, "not_found", "Virtual model not found.")
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	cutoff := periodCutoffString(r.URL.Query().Get("period"))
	if err := s.writeActivityCSV(w, r, "tiller-"+sanitizeFilename(canonical)+"-activity-"+time.Now().UTC().Format("2006-01-02")+".csv", func(fn func(store.ActivityRow) error) error {
		return sc.ExportVirtualActivity(r.Context(), modelID, canonical, search, cutoff, fn)
	}); err != nil && s.logger != nil {
		s.logger.Warn("activity export failed", "error_class", fmt.Sprintf("%T", err))
	}
}

// exportRealModelActivityCSV streams a CSV of activity attributable to a real
// model, honouring the active search filter. One inference request = one row.
func (s *Server) exportRealModelActivityCSV(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("id")
	sc := s.scope(r)
	provider, upstream, err := sc.RealModelCanonical(r.Context(), modelID)
	if err != nil {
		adminError(w, 404, "not_found", "Model not found.")
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	cutoff := periodCutoffString(r.URL.Query().Get("period"))
	if err := s.writeActivityCSV(w, r, "tiller-"+sanitizeFilename(provider+"/"+upstream)+"-activity-"+time.Now().UTC().Format("2006-01-02")+".csv", func(fn func(store.ActivityRow) error) error {
		return sc.ExportRealActivity(r.Context(), modelID, provider, upstream, search, cutoff, fn)
	}); err != nil && s.logger != nil {
		s.logger.Warn("activity export failed", "error_class", fmt.Sprintf("%T", err))
	}
}

// writeActivityCSV streams the export rows as a UTF-8 CSV attachment. Only
// metadata is written; unknown values stay blank. A BOM is prepended so Excel
// detects UTF-8 correctly.
func (s *Server) writeActivityCSV(w http.ResponseWriter, r *http.Request, filename string, run func(fn func(store.ActivityRow) error) error) error {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	write := func(record []string) error {
		if err := cw.Write(record); err != nil {
			return err
		}
		return cw.Error()
	}
	if err := write([]string{
		"timestamp", "client_key", "client_requested_model", "client_exposed_model",
		"virtual_model", "bound_target", "final_provider", "final_model", "protocol",
		"streaming", "http_status", "latency_ms", "input_tokens", "output_tokens",
		"cached_input_tokens", "cache_creation_input_tokens", "attempt_count", "fallback_used", "fallback_reason", "error_message", "request_body", "request_body_truncated", "error_body", "error_body_truncated",
		"provider_request_id", "client_request_id", "route_kind",
	}); err != nil {
		return err
	}
	written := 0
	if err := run(func(row store.ActivityRow) error {
		virtualModel := ""
		if row.RouteKind != nil && *row.RouteKind == "virtual" && row.RouteModel != nil {
			virtualModel = *row.RouteModel
		}
		if err := write([]string{
			row.CreatedAt, neutralizeCSVField(row.ClientName), neutralizeCSVField(row.RequestedModel),
			neutralizeCSVField(strPtrOrEmpty(row.ExposedModel)), neutralizeCSVField(virtualModel),
			neutralizeCSVField(strPtrOrEmpty(row.RouteModel)), neutralizeCSVField(strPtrOrEmpty(row.ResolvedProvider)),
			neutralizeCSVField(strPtrOrEmpty(row.ResolvedModel)), row.Protocol,
			strconv.FormatBool(row.Streaming), strconv.Itoa(row.HTTPStatus), strconv.FormatInt(row.LatencyMs, 10),
			int64PtrOrEmpty(row.InputTokens), int64PtrOrEmpty(row.OutputTokens), int64PtrOrEmpty(row.CacheReadInputTokens),
			int64PtrOrEmpty(row.CacheCreationInputTokens), strconv.Itoa(row.AttemptCount), strconv.FormatBool(row.FallbackUsed),
			strPtrOrEmpty(row.FallbackReason), neutralizeCSVField(strPtrOrEmpty(row.ErrorMessage)),
			neutralizeCSVField(strPtrOrEmpty(row.RequestBody)), strconv.FormatBool(row.RequestBodyTruncated),
			neutralizeCSVField(strPtrOrEmpty(row.ErrorBody)), strconv.FormatBool(row.ErrorBodyTruncated),
			neutralizeCSVField(strPtrOrEmpty(row.ProviderRequestID)), row.ClientRequestID, strPtrOrEmpty(row.RouteKind),
		}); err != nil {
			return err
		}
		written++
		if written%64 == 0 {
			cw.Flush()
			return cw.Error()
		}
		return nil
	}); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

func (s *Server) listVirtualActivity(w http.ResponseWriter, r *http.Request) {
	_ = s.flushActivity(r.Context())
	modelID := r.PathValue("id")
	sc := s.scope(r)
	canonical, err := sc.VirtualModelCanonical(r.Context(), modelID)
	if err != nil {
		adminError(w, 404, "not_found", "Virtual model not found.")
		return
	}
	limit, offset, search := pagination(r)
	rows, err := sc.ListVirtualActivity(r.Context(), modelID, canonical, search, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	data := make([]globalActivityView, 0, len(rows))
	for i := range rows {
		data = append(data, globalActivityView{activityView: activityViewFromStore(rows[i]), ClientKeyID: rows[i].ClientKeyID, ClientName: rows[i].ClientName})
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func (s *Server) listRealModelActivity(w http.ResponseWriter, r *http.Request) {
	_ = s.flushActivity(r.Context())
	modelID := r.PathValue("id")
	sc := s.scope(r)
	provider, upstream, err := sc.RealModelCanonical(r.Context(), modelID)
	if err != nil {
		adminError(w, 404, "not_found", "Model not found.")
		return
	}
	limit, offset, search := pagination(r)
	rows, err := sc.ListRealModelActivity(r.Context(), modelID, provider, upstream, search, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	data := make([]globalActivityView, 0, len(rows))
	for i := range rows {
		data = append(data, globalActivityView{activityView: activityViewFromStore(rows[i]), ClientKeyID: rows[i].ClientKeyID, ClientName: rows[i].ClientName})
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

func strPtrOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func int64PtrOrEmpty(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

// sanitizeFilename strips characters that are unsafe in a Content-Disposition
// filename, replacing slashes (as in virtual canonical ids like "main/coding")
// with dashes.
func sanitizeFilename(s string) string {
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	return strings.Map(func(r rune) rune {
		if r < 32 || r == '"' || r == ':' || r == '*' || r == '?' || r == '<' || r == '>' || r == '|' {
			return '-'
		}
		return r
	}, s)
}

// neutralizeCSVField prevents spreadsheet formula injection by escaping a
// leading formula prefix (= + - @ tab or carriage return) with a single quote.
func neutralizeCSVField(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// periodCutoffString returns the UTC cutoff for a CSV export period as an
// RFC3339Nano string, or "" when no period filter applies.
func periodCutoffString(period string) string {
	switch strings.TrimSpace(period) {
	case "24h":
		return time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	case "7d":
		return time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)
	case "30d":
		return time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)
	default:
		return ""
	}
}
