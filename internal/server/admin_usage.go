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
	realModels, err := s.usageByReal(r, cut1h, cut24h, cut7d)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load usage.")
		return
	}
	writeJSON(w, 200, map[string]any{
		"client_keys":    clientKeys,
		"virtual_models": virtualModels,
		"real_models":    realModels,
	})
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
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT requested_model, `+usageSelect+` FROM request_logs WHERE created_at >= ? GROUP BY requested_model`, c1, c24, c7, c7)
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
