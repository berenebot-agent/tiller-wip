package server

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	AttemptCount             int     `json:"attempt_count"`
	FallbackUsed             bool    `json:"fallback_used"`
	FallbackReason           *string `json:"fallback_reason"`
	CreatedAt                string  `json:"created_at"`
}

// scanActivityRow scans one request_logs row into v. When withClient is true the
// query additionally selects rl.client_key_id (for global activity) or ck.name
// (for exports) as the final column; clientKeyID and clientName are then
// populated. The streaming/fallback int columns are converted to bools here so
// every scan loop shares the same single column list.
func scanActivityRow(scan func(dest ...any) error, v *activityView, withClient bool, clientKeyID, clientName *string) error {
	var streaming, fallback int
	dest := []any{&v.ID, &v.RequestedModel, &v.ExposedModel, &v.RouteKind, &v.RouteModelID, &v.RouteModel, &v.ResolvedProvider, &v.ResolvedModel, &v.Protocol, &streaming, &v.HTTPStatus, &v.LatencyMs, &v.InputTokens, &v.OutputTokens, &v.CacheReadInputTokens, &v.CacheCreationInputTokens, &v.ProviderRequestID, &v.ClientRequestID, &v.ErrorText, &v.AttemptCount, &fallback, &v.FallbackReason, &v.CreatedAt}
	if withClient {
		if clientKeyID != nil {
			dest = append(dest, clientKeyID)
		}
		if clientName != nil {
			dest = append(dest, clientName)
		}
	}
	if err := scan(dest...); err != nil {
		return err
	}
	v.Streaming, v.FallbackUsed = scanBool(streaming), scanBool(fallback)
	return nil
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
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT id,requested_model,exposed_model,route_kind,route_model_id,route_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,provider_request_id,client_request_id,error_text,attempt_count,fallback_used,fallback_reason,created_at FROM request_logs WHERE client_key_id=? AND (requested_model LIKE ? OR coalesce(exposed_model,'') LIKE ? OR coalesce(route_model,'') LIKE ? OR coalesce(resolved_provider,'') LIKE ? OR CAST(http_status AS TEXT) LIKE ? OR coalesce(error_text,'') LIKE ?) ORDER BY created_at DESC LIMIT ? OFFSET ?`, clientID, pattern, pattern, pattern, pattern, pattern, pattern, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	defer rows.Close()
	data := []activityView{}
	for rows.Next() {
		var v activityView
		if err := scanActivityRow(rows.Scan, &v, false, nil, nil); err != nil {
			adminError(w, 500, "database_error", "Could not load activity.")
			return
		}
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
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT rl.id,rl.requested_model,rl.exposed_model,rl.route_kind,rl.route_model_id,rl.route_model,rl.resolved_provider,rl.resolved_model,rl.protocol,rl.streaming,rl.http_status,rl.latency_ms,rl.input_tokens,rl.output_tokens,rl.cache_read_input_tokens,rl.cache_creation_input_tokens,rl.provider_request_id,rl.client_request_id,rl.error_text,rl.attempt_count,rl.fallback_used,rl.fallback_reason,rl.created_at,rl.client_key_id,ck.name FROM request_logs rl JOIN client_keys ck ON ck.id=rl.client_key_id WHERE (ck.name LIKE ? OR rl.requested_model LIKE ? OR coalesce(rl.exposed_model,'') LIKE ? OR coalesce(rl.route_model,'') LIKE ? OR coalesce(rl.resolved_provider,'') LIKE ? OR coalesce(rl.resolved_model,'') LIKE ? OR coalesce(rl.resolved_provider,'') || '/' || coalesce(rl.resolved_model,'') LIKE ? OR CAST(rl.http_status AS TEXT) LIKE ? OR rl.client_request_id LIKE ? OR coalesce(rl.provider_request_id,'') LIKE ? OR coalesce(rl.error_text,'') LIKE ?) ORDER BY rl.created_at DESC, rl.id DESC LIMIT ? OFFSET ?`, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, limit, offset)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	defer rows.Close()
	data := []globalActivityView{}
	for rows.Next() {
		var v globalActivityView
		if err := scanActivityRow(rows.Scan, &v.activityView, true, &v.ClientKeyID, &v.ClientName); err != nil {
			adminError(w, 500, "database_error", "Could not load activity.")
			return
		}
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

// activityExportRow is the metadata captured for one CSV row. It mirrors the
// request_logs columns plus the owning client's name (via JOIN) so both the
// client-scoped and virtual-model-scoped exports can include it.
type activityExportRow struct {
	activityView
	ClientName string
}

// queryActivityExport runs a metadata-only SELECT over request_logs joined to
// client_keys, applying the given WHERE clause and optional search pattern, and
// returns one row per inference request ordered newest-first. It is shared by
// the client-key and virtual-model CSV export handlers so the column set cannot
// drift between them.
func (s *Server) queryActivityExport(ctx context.Context, where string, args []any) ([]activityExportRow, error) {
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT rl.id,rl.requested_model,rl.exposed_model,rl.route_kind,rl.route_model_id,rl.route_model,rl.resolved_provider,rl.resolved_model,rl.protocol,rl.streaming,rl.http_status,rl.latency_ms,rl.input_tokens,rl.output_tokens,rl.cache_read_input_tokens,rl.cache_creation_input_tokens,rl.provider_request_id,rl.client_request_id,rl.error_text,rl.attempt_count,rl.fallback_used,rl.fallback_reason,rl.created_at,ck.name FROM request_logs rl JOIN client_keys ck ON ck.id=rl.client_key_id WHERE `+where+` ORDER BY rl.created_at DESC, rl.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []activityExportRow{}
	for rows.Next() {
		var v activityExportRow
		if err := scanActivityRow(rows.Scan, &v.activityView, true, nil, &v.ClientName); err != nil {
			return nil, err
		}
		data = append(data, v)
	}
	return data, rows.Err()
}

// exportClientActivityCSV streams a metadata-only CSV of the client key's
// activity, honouring the active search filter. One inference request = one row.
func (s *Server) exportClientActivityCSV(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	var name string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT name FROM client_keys WHERE id=?`, clientID).Scan(&name); err != nil {
		adminError(w, 404, "not_found", "Client key not found.")
		return
	}
	where := `rl.client_key_id=?`
	args := []any{clientID}
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		pattern := "%" + search + "%"
		where += ` AND (rl.requested_model LIKE ? OR coalesce(rl.exposed_model,'') LIKE ? OR coalesce(rl.route_model,'') LIKE ? OR coalesce(rl.resolved_provider,'') LIKE ? OR CAST(rl.http_status AS TEXT) LIKE ? OR coalesce(rl.error_text,'') LIKE ?)`
		args = append(args, pattern, pattern, pattern, pattern, pattern, pattern)
	}
	where, args = applyExportPeriod(where, args, r.URL.Query().Get("period"))
	rows, err := s.queryActivityExport(r.Context(), where, args)
	if err != nil {
		adminError(w, 500, "database_error", "Could not export activity.")
		return
	}
	writeActivityCSV(w, r, "tiller-"+sanitizeFilename(name)+"-activity-"+time.Now().UTC().Format("2006-01-02")+".csv", rows)
}

// exportVirtualActivityCSV streams a metadata-only CSV of activity attributable
// to a virtual model, honouring the active search filter. It matches new rows by
// route_model_id and legacy rows (route_kind NULL) by canonical name. One
// inference request = one row.
func (s *Server) exportVirtualActivityCSV(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("id")
	var canonical string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT g.name||'/'||v.name FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE v.id=?`, modelID).Scan(&canonical); err != nil {
		adminError(w, 404, "not_found", "Virtual model not found.")
		return
	}
	where, args := virtualAttribution(modelID, canonical)
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		pattern := "%" + search + "%"
		where += ` AND (rl.requested_model LIKE ? OR coalesce(rl.exposed_model,'') LIKE ? OR coalesce(rl.route_model,'') LIKE ? OR coalesce(rl.resolved_provider,'') LIKE ? OR CAST(rl.http_status AS TEXT) LIKE ? OR coalesce(rl.error_text,'') LIKE ?)`
		args = append(args, pattern, pattern, pattern, pattern, pattern, pattern)
	}
	where, args = applyExportPeriod(where, args, r.URL.Query().Get("period"))
	rows, err := s.queryActivityExport(r.Context(), where, args)
	if err != nil {
		adminError(w, 500, "database_error", "Could not export activity.")
		return
	}
	writeActivityCSV(w, r, "tiller-"+sanitizeFilename(canonical)+"-activity-"+time.Now().UTC().Format("2006-01-02")+".csv", rows)
}

// exportRealModelActivityCSV streams a metadata-only CSV of activity that
// resolved to a real model (resolved_provider + resolved_model), honouring the
// active search filter. Scoping by resolved names keeps legacy rows and
// virtual-routed requests visible. One inference request = one row.
func (s *Server) exportRealModelActivityCSV(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("id")
	var provider, upstream string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT p.name,m.upstream_model_id FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE m.id=?`, modelID).Scan(&provider, &upstream); err != nil {
		adminError(w, 404, "not_found", "Model not found.")
		return
	}
	where, args := realAttribution(provider, upstream)
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		pattern := "%" + search + "%"
		where += ` AND (rl.requested_model LIKE ? OR coalesce(rl.exposed_model,'') LIKE ? OR coalesce(rl.route_model,'') LIKE ? OR coalesce(rl.resolved_provider,'') LIKE ? OR CAST(rl.http_status AS TEXT) LIKE ? OR coalesce(rl.error_text,'') LIKE ?)`
		args = append(args, pattern, pattern, pattern, pattern, pattern, pattern)
	}
	where, args = applyExportPeriod(where, args, r.URL.Query().Get("period"))
	rows, err := s.queryActivityExport(r.Context(), where, args)
	if err != nil {
		adminError(w, 500, "database_error", "Could not export activity.")
		return
	}
	writeActivityCSV(w, r, "tiller-"+sanitizeFilename(provider+"/"+upstream)+"-activity-"+time.Now().UTC().Format("2006-01-02")+".csv", rows)
}

// writeActivityCSV streams the export rows as a UTF-8 CSV attachment. Only
// metadata is written; unknown values stay blank. A BOM is prepended so Excel
// detects UTF-8 correctly.
func writeActivityCSV(w http.ResponseWriter, r *http.Request, filename string, rows []activityExportRow) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte("\xEF\xBB\xBF")) // UTF-8 BOM for Excel
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"timestamp", "client_key", "client_requested_model", "client_exposed_model",
		"virtual_model", "bound_target", "final_provider", "final_model", "protocol",
		"streaming", "http_status", "latency_ms", "input_tokens", "output_tokens",
		"cached_input_tokens", "cache_creation_input_tokens", "attempt_count", "fallback_used", "fallback_reason",
		"provider_request_id", "client_request_id", "route_kind",
	})
	for _, row := range rows {
		virtualModel := ""
		if row.RouteKind != nil && *row.RouteKind == "virtual" && row.RouteModel != nil {
			virtualModel = *row.RouteModel
		}
		boundTarget := ""
		if row.RouteModel != nil {
			boundTarget = *row.RouteModel
		}
		_ = cw.Write([]string{
			row.CreatedAt,
			neutralizeCSVField(row.ClientName),
			neutralizeCSVField(row.RequestedModel),
			neutralizeCSVField(strPtrOrEmpty(row.ExposedModel)),
			neutralizeCSVField(virtualModel),
			neutralizeCSVField(boundTarget),
			neutralizeCSVField(strPtrOrEmpty(row.ResolvedProvider)),
			neutralizeCSVField(strPtrOrEmpty(row.ResolvedModel)),
			row.Protocol,
			strconv.FormatBool(row.Streaming),
			strconv.Itoa(row.HTTPStatus),
			strconv.FormatInt(row.LatencyMs, 10),
			int64PtrOrEmpty(row.InputTokens),
			int64PtrOrEmpty(row.OutputTokens),
			int64PtrOrEmpty(row.CacheReadInputTokens),
			int64PtrOrEmpty(row.CacheCreationInputTokens),
			strconv.Itoa(row.AttemptCount),
			strconv.FormatBool(row.FallbackUsed),
			strPtrOrEmpty(row.FallbackReason),
			neutralizeCSVField(strPtrOrEmpty(row.ProviderRequestID)),
			row.ClientRequestID,
			strPtrOrEmpty(row.RouteKind),
		})
	}
	cw.Flush()
}

// listScopedActivity returns metadata for requests matching the given WHERE
// clause (e.g. a specific real or virtual model), newest first, with the same
// search and pagination as the client-key activity endpoint. It verifies the
// scoping entity exists first.
func (s *Server) listScopedActivity(w http.ResponseWriter, r *http.Request, where string, args []any, existsQuery string, existsArgs []any, notFoundMsg string) {
	var exists int
	if err := s.db.SQL.QueryRowContext(r.Context(), existsQuery, existsArgs...).Scan(&exists); err != nil || exists == 0 {
		adminError(w, 404, "not_found", notFoundMsg)
		return
	}
	limit, offset, search := pagination(r)
	pattern := "%" + search + "%"
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, pattern, pattern, pattern, pattern, pattern, pattern, limit, offset)
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT rl.id,rl.requested_model,rl.exposed_model,rl.route_kind,rl.route_model_id,rl.route_model,rl.resolved_provider,rl.resolved_model,rl.protocol,rl.streaming,rl.http_status,rl.latency_ms,rl.input_tokens,rl.output_tokens,rl.cache_read_input_tokens,rl.cache_creation_input_tokens,rl.provider_request_id,rl.client_request_id,rl.error_text,rl.attempt_count,rl.fallback_used,rl.fallback_reason,rl.created_at,rl.client_key_id,ck.name FROM request_logs rl JOIN client_keys ck ON ck.id=rl.client_key_id WHERE `+where+` AND (rl.requested_model LIKE ? OR coalesce(rl.exposed_model,'') LIKE ? OR coalesce(rl.route_model,'') LIKE ? OR coalesce(rl.resolved_provider,'') LIKE ? OR CAST(rl.http_status AS TEXT) LIKE ? OR coalesce(rl.error_text,'') LIKE ?) ORDER BY rl.created_at DESC, rl.id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	defer rows.Close()
	data := []globalActivityView{}
	for rows.Next() {
		var v globalActivityView
		if err := scanActivityRow(rows.Scan, &v.activityView, true, &v.ClientKeyID, &v.ClientName); err != nil {
			adminError(w, 500, "database_error", "Could not load activity.")
			return
		}
		data = append(data, v)
	}
	if err := rows.Err(); err != nil {
		adminError(w, 500, "database_error", "Could not load activity.")
		return
	}
	writeJSON(w, 200, map[string]any{"data": data, "limit": limit, "offset": offset})
}

// listVirtualActivity returns metadata for requests attributable to a virtual
// model, newest first, with the same search and pagination as the client-key
// activity endpoint. It matches new rows by route_model_id and legacy rows
// (route_kind NULL) by the virtual model's canonical name.
func (s *Server) listVirtualActivity(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("id")
	var canonical string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT g.name||'/'||v.name FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE v.id=?`, modelID).Scan(&canonical); err != nil {
		adminError(w, 404, "not_found", "Virtual model not found.")
		return
	}
	where, args := virtualAttribution(modelID, canonical)
	s.listScopedActivity(w, r, where, args, `SELECT count(*) FROM virtual_models WHERE id=?`, []any{modelID}, "Virtual model not found.")
}

// listRealModelActivity returns metadata for requests that resolved to a real
// model (resolved_provider + resolved_model), newest first, with the same search
// and pagination as the client-key activity endpoint. Scoping by resolved names
// (not route_model_id) keeps legacy rows and virtual-routed requests visible.
func (s *Server) listRealModelActivity(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("id")
	var provider, upstream string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT p.name,m.upstream_model_id FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE m.id=?`, modelID).Scan(&provider, &upstream); err != nil {
		adminError(w, 404, "not_found", "Model not found.")
		return
	}
	where, args := realAttribution(provider, upstream)
	s.listScopedActivity(w, r, where, args, `SELECT count(*) FROM provider_models WHERE id=?`, []any{modelID}, "Model not found.")
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

// periodCutoff returns the UTC cutoff for a CSV export period, and whether a
// filter applies. "all" (or empty/unknown) returns no filter.
func periodCutoff(period string) (time.Time, bool) {
	switch strings.TrimSpace(period) {
	case "24h":
		return time.Now().UTC().Add(-24 * time.Hour), true
	case "7d":
		return time.Now().UTC().Add(-7 * 24 * time.Hour), true
	case "30d":
		return time.Now().UTC().Add(-30 * 24 * time.Hour), true
	default:
		return time.Time{}, false
	}
}

// applyExportPeriod appends a created_at cutoff to the export WHERE clause for
// the given period query param. Returns the updated where/args.
func applyExportPeriod(where string, args []any, period string) (string, []any) {
	if cutoff, ok := periodCutoff(period); ok {
		where += ` AND rl.created_at >= ?`
		args = append(args, cutoff.Format(time.RFC3339Nano))
	}
	return where, args
}
