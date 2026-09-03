package server

import (
	"database/sql"
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
	clientCache, err := s.cacheByClient(r, cut1h, cut24h, cut7d)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load usage.")
		return
	}
	virtualCache, err := s.cacheByVirtual(r, cut1h, cut24h, cut7d)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load usage.")
		return
	}
	realCache, err := s.cacheByReal(r, cut1h, cut24h, cut7d)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load usage.")
		return
	}
	writeJSON(w, 200, map[string]any{
		"client_keys":         clientKeys,
		"virtual_models":      virtualModels,
		"target_health":       targetHealth,
		"real_models":         realModels,
		"client_cache":        clientCache,
		"virtual_cache":       virtualCache,
		"real_cache":          realCache,
		"target_last_outcome": s.lastOutcomeSnapshot(),
	})
}

// lastOutcomeSnapshot returns a copy of the in-memory per-real-model last
// request outcomes, keyed by "provider_name/upstream_model_id".
func (s *Server) lastOutcomeSnapshot() map[string]lastOutcome {
	s.lastOutcomeMu.RLock()
	defer s.lastOutcomeMu.RUnlock()
	out := make(map[string]lastOutcome, len(s.lastOutcome))
	for k, v := range s.lastOutcome {
		out[k] = v
	}
	return out
}

// targetResolutionHealth reports request outcomes for each target that was
// attempted (final attempt resolution + every fallback attempt).
//
// The final, resolved target is read from request_logs (resolved_provider,
// resolved_model — populated for the row that ultimately served the
// request). Every attempted target, including ones that failed and were
// then replaced by a fallback, is read from request_attempts. This covers
// two cases the request_logs row alone misses:
//
//   - A fallback target that failed but where a later target succeeded:
//     the row's resolved_provider/resolved_model point at the later target,
//     so the failed target would otherwise be invisible.
//   - A virtual model whose entire fallback chain exhausted: the row is
//     logged with NULL resolved_provider/resolved_model, so neither the
//     failed nor any other target appears in target_health without the
//     attempts join.
//
// A successful attempt is an HTTP 2xx response; every other recorded
// response is treated as a failure. Logs are metadata-only and may be
// disabled per key.
func (s *Server) targetResolutionHealth(r *http.Request, c1, c24 string) (map[string]targetResolutionHealth, error) {
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT key,
		max(success_1h), max(failure_1h), max(success_24h)
		FROM (
			SELECT l.resolved_provider||'/'||l.resolved_model AS key,
				CASE WHEN l.created_at >= ? AND l.http_status >= 200 AND l.http_status < 300 THEN 1 ELSE 0 END AS success_1h,
				CASE WHEN l.created_at >= ? AND NOT (l.http_status >= 200 AND l.http_status < 300) THEN 1 ELSE 0 END AS failure_1h,
				CASE WHEN l.http_status >= 200 AND l.http_status < 300 THEN 1 ELSE 0 END AS success_24h
			FROM request_logs l
			JOIN (SELECT v.id,g.name||'/'||v.name AS canonical FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id) vm
			  ON `+virtualAttributionJoin()+`
			WHERE l.created_at >= ? AND l.resolved_provider IS NOT NULL AND l.resolved_model IS NOT NULL
			UNION ALL
			SELECT a.provider||'/'||a.model AS key,
				CASE WHEN a.created_at >= ? AND a.result='success' THEN 1 ELSE 0 END AS success_1h,
				CASE WHEN a.created_at >= ? AND a.result IN ('failed','skipped') THEN 1 ELSE 0 END AS failure_1h,
				CASE WHEN a.result='success' THEN 1 ELSE 0 END AS success_24h
			FROM request_attempts a
			WHERE a.created_at >= ?
		)
		GROUP BY key`, c1, c1, c24, c1, c1, c24)
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
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT vm.canonical, `+usageSelect+` FROM request_logs l JOIN (SELECT v.id,g.name||'/'||v.name AS canonical FROM virtual_models v JOIN virtual_provider_groups g ON g.id = v.virtual_group_id) vm ON `+virtualAttributionJoin()+` WHERE l.created_at >= ? GROUP BY vm.canonical`, c1, c24, c7, c7)
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

// cacheWindows holds prompt-cache hit percentages (0-100) for the three
// lookback windows. Values are nil when no cache data was recorded, so callers
// can render "—" rather than a fabricated 0%.
type cacheWindows struct {
	H1  *float64 `json:"1h"`
	H24 *float64 `json:"24h"`
	D7  *float64 `json:"7d"`
}

// cachePct computes cache_read / total_input as a percentage. Both read and
// input must be present and input non-zero; otherwise nil signals "no cache
// data", never a misleading 0.
func cachePct(read, input sql.NullFloat64) *float64 {
	if !read.Valid || !input.Valid || input.Float64 <= 0 {
		return nil
	}
	p := read.Float64 / input.Float64 * 100
	return &p
}

// cacheSelect returns, per window, the summed cache_read_input_tokens and
// input_tokens restricted to cache-valid rows (those that actually reported
// prompt-cache data). Both sums are NULL when a window has no cache-valid row,
// so callers can render "n.a." rather than a misleading 0%.
const cacheSelect = `sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN cache_read_input_tokens ELSE 0 END),
	sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN input_tokens ELSE 0 END),
	sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN cache_read_input_tokens ELSE 0 END),
	sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN input_tokens ELSE 0 END),
	sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN cache_read_input_tokens ELSE 0 END),
	sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN input_tokens ELSE 0 END)`

func (s *Server) cacheByClient(r *http.Request, c1, c24, c7 string) (map[string]cacheWindows, error) {
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT client_key_id, `+cacheSelect+` FROM request_logs WHERE created_at >= ? GROUP BY client_key_id`, c1, c1, c24, c24, c7, c7, c7)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCache(rows, 0)
}

func (s *Server) cacheByVirtual(r *http.Request, c1, c24, c7 string) (map[string]cacheWindows, error) {
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT vm.canonical, `+cacheSelect+` FROM request_logs l JOIN (SELECT v.id,g.name||'/'||v.name AS canonical FROM virtual_models v JOIN virtual_provider_groups g ON g.id = v.virtual_group_id) vm ON `+virtualAttributionJoin()+` WHERE l.created_at >= ? GROUP BY vm.canonical`, c1, c1, c24, c24, c7, c7, c7)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCache(rows, 0)
}

func (s *Server) cacheByReal(r *http.Request, c1, c24, c7 string) (map[string]cacheWindows, error) {
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT resolved_provider, resolved_model, `+cacheSelect+` FROM request_logs WHERE created_at >= ? AND resolved_provider IS NOT NULL GROUP BY resolved_provider, resolved_model`, c1, c1, c24, c24, c7, c7, c7)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCache(rows, 1)
}

// scanCache scans the windowed cache read/input sums into each row's keyed
// cacheWindows. keyCols is the number of leading group-by columns preceding the
// cache sums (0 for client/virtual, 1 for the composite provider/model key).
func scanCache(rows *sql.Rows, keyCols int) (map[string]cacheWindows, error) {
	out := map[string]cacheWindows{}
	var cols []any
	if keyCols == 1 {
		cols = []any{new(string), new(string)}
	} else {
		cols = []any{new(string)}
	}
	var sums [6]sql.NullFloat64
	for i := range sums {
		cols = append(cols, &sums[i])
	}
	for rows.Next() {
		var key string
		if keyCols == 1 {
			var provider, model string
			cols[0] = &provider
			cols[1] = &model
			if err := rows.Scan(cols...); err != nil {
				return nil, err
			}
			key = provider + "/" + model
		} else {
			if err := rows.Scan(cols...); err != nil {
				return nil, err
			}
			key = *cols[0].(*string)
		}
		out[key] = cacheWindows{
			H1:  cachePct(sums[0], sums[1]),
			H24: cachePct(sums[2], sums[3]),
			D7:  cachePct(sums[4], sums[5]),
		}
	}
	return out, rows.Err()
}
