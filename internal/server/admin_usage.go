package server

import (
	"net/http"
	"time"
)

// usageWindows holds total tokens (input + output) for the three lookback
// windows surfaced in the table views.
type usageWindows struct {
	H1  int64 `json:"1h"`
	H24 int64 `json:"24h"`
	D7  int64 `json:"7d"`
}

type targetResolutionHealth struct {
	Success1h  bool `json:"success_1h"`
	Failure1h  bool `json:"failure_1h"`
	Success24h bool `json:"success_24h"`
}

// usage returns token totals per client key, virtual model, and real model for
// the last hour, last 24 hours, and last week. Read-only aggregation over
// request_logs; no cost/pricing.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	cut1h := now.Add(-time.Hour).Format(time.RFC3339Nano)
	cut24h := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	cut7d := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)

	clientKeys, err := s.usageByClient(r, cut1h, cut24h, cut7d)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load usage.")
		return
	}
	virtualModels, err := s.usageByVirtual(r, cut1h, cut24h, cut7d)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load usage.")
		return
	}
	targetHealth, err := s.targetResolutionHealth(r, cut1h, cut24h)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load usage.")
		return
	}
	realModels, err := s.usageByReal(r, cut1h, cut24h, cut7d)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load usage.")
		return
	}
	writeJSON(w, 200, map[string]any{
		"client_keys":    clientKeys,
		"virtual_models": virtualModels,
		"target_health":  targetHealth,
		"real_models":    realModels,
	})
}

// targetResolutionHealth reports request outcomes for each resolved target
// used by a virtual identity.
// A successful request is an HTTP 2xx response; every other recorded response
// is treated as a failure. Logs are metadata-only and may be disabled per key.
func (s *Server) targetResolutionHealth(r *http.Request, c1, c24 string) (map[string]targetResolutionHealth, error) {
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT l.resolved_provider||'/'||l.resolved_model,
		max(CASE WHEN l.created_at >= ? AND l.http_status >= 200 AND l.http_status < 300 THEN 1 ELSE 0 END),
		max(CASE WHEN l.created_at >= ? AND NOT (l.http_status >= 200 AND l.http_status < 300) THEN 1 ELSE 0 END),
		max(CASE WHEN l.http_status >= 200 AND l.http_status < 300 THEN 1 ELSE 0 END)
		FROM request_logs l
		JOIN (SELECT g.name||'/'||v.name AS canonical FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id) vm ON vm.canonical = l.requested_model
		WHERE l.created_at >= ? AND l.resolved_provider IS NOT NULL AND l.resolved_model IS NOT NULL
		GROUP BY l.resolved_provider, l.resolved_model`, c1, c1, c24)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]targetResolutionHealth{}
	for rows.Next() {
		var id string
		var success1h, failure1h, success24h int
		if err := rows.Scan(&id, &success1h, &failure1h, &success24h); err != nil {
			return nil, err
		}
		out[id] = targetResolutionHealth{Success1h: success1h == 1, Failure1h: failure1h == 1, Success24h: success24h == 1}
	}
	return out, rows.Err()
}

const usageSelect = `coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0)+coalesce(output_tokens,0) ELSE 0 END),0),
	coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0)+coalesce(output_tokens,0) ELSE 0 END),0),
	coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0)+coalesce(output_tokens,0) ELSE 0 END),0)`

func (s *Server) usageByClient(r *http.Request, c1, c24, c7 string) (map[string]usageWindows, error) {
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT client_key_id, `+usageSelect+` FROM request_logs WHERE created_at >= ? GROUP BY client_key_id`, c1, c24, c7, c7)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]usageWindows{}
	for rows.Next() {
		var id string
		var w usageWindows
		if err := rows.Scan(&id, &w.H1, &w.H24, &w.D7); err != nil {
			return nil, err
		}
		out[id] = w
	}
	return out, rows.Err()
}

func (s *Server) usageByVirtual(r *http.Request, c1, c24, c7 string) (map[string]usageWindows, error) {
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT vm.canonical, `+usageSelect+` FROM request_logs l JOIN (SELECT g.name||'/'||v.name AS canonical FROM virtual_models v JOIN virtual_provider_groups g ON g.id = v.virtual_group_id) vm ON vm.canonical = l.requested_model WHERE l.created_at >= ? GROUP BY vm.canonical`, c1, c24, c7, c7)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]usageWindows{}
	for rows.Next() {
		var id string
		var w usageWindows
		if err := rows.Scan(&id, &w.H1, &w.H24, &w.D7); err != nil {
			return nil, err
		}
		out[id] = w
	}
	return out, rows.Err()
}

func (s *Server) usageByReal(r *http.Request, c1, c24, c7 string) (map[string]usageWindows, error) {
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT resolved_provider, resolved_model, `+usageSelect+` FROM request_logs WHERE created_at >= ? AND resolved_provider IS NOT NULL GROUP BY resolved_provider, resolved_model`, c1, c24, c7, c7)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]usageWindows{}
	for rows.Next() {
		var provider, model string
		var w usageWindows
		if err := rows.Scan(&provider, &model, &w.H1, &w.H24, &w.D7); err != nil {
			return nil, err
		}
		out[provider+"/"+model] = w
	}
	return out, rows.Err()
}
