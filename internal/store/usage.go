package store

import (
	"context"
	"database/sql"
)

// UsageWindows holds total tokens (input + output) for the three lookback
// windows surfaced in the table views.
type UsageWindows struct {
	H1  int64 `json:"1h"`
	H24 int64 `json:"24h"`
	D7  int64 `json:"7d"`
}

// CacheWindows holds prompt-cache hit percentages (0-100) for the three
// lookback windows. Values are nil when no cache data was recorded.
type CacheWindows struct {
	H1  *float64 `json:"1h"`
	H24 *float64 `json:"24h"`
	D7  *float64 `json:"7d"`
}

// TargetHealth reports request outcomes for one attempted target.
type TargetHealth struct {
	Success1h  bool `json:"success_1h"`
	Failure1h  bool `json:"failure_1h"`
	Success24h bool `json:"success_24h"`
}

const usageSelect = `coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0)+coalesce(output_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0)+coalesce(output_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0)+coalesce(output_tokens,0) ELSE 0 END),0)`

// virtualAttributionFilter matches request_logs rows attributable to a virtual
// model without joining the control-plane tables. The canonical name is
// captured on the row at resolution time (route_model), so the Activity file
// is self-contained; a renamed virtual model's history stays under the name it
// had when the request was served.
const virtualAttributionFilter = `(route_kind='virtual' OR (route_kind IS NULL AND route_status='legacy')) AND route_model IS NOT NULL`

func (s *Scope) UsageByClient(ctx context.Context, c1, c24, c7 string) (map[string]UsageWindows, error) {
	var out map[string]UsageWindows
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT client_key_id, `+usageSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? GROUP BY client_key_id`, c1, c24, c7, s.accountID, c7)
		if err != nil {
			return err
		}
		out, err = scanUsage(rows, 0)
		return err
	})
	return out, err
}

func (s *Scope) UsageByVirtual(ctx context.Context, c1, c24, c7 string) (map[string]UsageWindows, error) {
	var out map[string]UsageWindows
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT route_model, `+usageSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? AND `+virtualAttributionFilter+` GROUP BY route_model`, c1, c24, c7, s.accountID, c7)
		if err != nil {
			return err
		}
		out, err = scanUsage(rows, 0)
		return err
	})
	return out, err
}

func (s *Scope) UsageByReal(ctx context.Context, c1, c24, c7 string) (map[string]UsageWindows, error) {
	var out map[string]UsageWindows
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT resolved_provider, resolved_model, `+usageSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? AND resolved_provider IS NOT NULL GROUP BY resolved_provider, resolved_model`, c1, c24, c7, s.accountID, c7)
		if err != nil {
			return err
		}
		out, err = scanUsage(rows, 1)
		return err
	})
	return out, err
}

// scanUsage scans the windowed usage sums. keyCols is the number of leading
// group-by columns (0 for client/virtual, 1 for provider+model).
func scanUsage(rows *sql.Rows, keyCols int) (map[string]UsageWindows, error) {
	defer rows.Close()
	out := map[string]UsageWindows{}
	for rows.Next() {
		var key, provider, model string
		var w UsageWindows
		if keyCols == 1 {
			if err := rows.Scan(&provider, &model, &w.H1, &w.H24, &w.D7); err != nil {
				return nil, err
			}
			key = provider + "/" + model
		} else {
			if err := rows.Scan(&key, &w.H1, &w.H24, &w.D7); err != nil {
				return nil, err
			}
		}
		out[key] = w
	}
	return out, rows.Err()
}

const cacheSelect = `sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN cache_read_input_tokens ELSE 0 END),
sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN input_tokens ELSE 0 END),
sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN cache_read_input_tokens ELSE 0 END),
sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN input_tokens ELSE 0 END),
sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN cache_read_input_tokens ELSE 0 END),
sum(CASE WHEN created_at >= ? AND cache_read_input_tokens IS NOT NULL THEN input_tokens ELSE 0 END)`

func (s *Scope) CacheByClient(ctx context.Context, c1, c24, c7 string) (map[string]CacheWindows, error) {
	var out map[string]CacheWindows
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT client_key_id, `+cacheSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? GROUP BY client_key_id`, c1, c1, c24, c24, c7, c7, s.accountID, c7)
		if err != nil {
			return err
		}
		out, err = scanCache(rows, 0)
		return err
	})
	return out, err
}

func (s *Scope) CacheByVirtual(ctx context.Context, c1, c24, c7 string) (map[string]CacheWindows, error) {
	var out map[string]CacheWindows
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT route_model, `+cacheSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? AND `+virtualAttributionFilter+` GROUP BY route_model`, c1, c1, c24, c24, c7, c7, s.accountID, c7)
		if err != nil {
			return err
		}
		out, err = scanCache(rows, 0)
		return err
	})
	return out, err
}

func (s *Scope) CacheByReal(ctx context.Context, c1, c24, c7 string) (map[string]CacheWindows, error) {
	var out map[string]CacheWindows
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT resolved_provider, resolved_model, `+cacheSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? AND resolved_provider IS NOT NULL GROUP BY resolved_provider, resolved_model`, c1, c1, c24, c24, c7, c7, s.accountID, c7)
		if err != nil {
			return err
		}
		out, err = scanCache(rows, 1)
		return err
	})
	return out, err
}

func scanCache(rows *sql.Rows, keyCols int) (map[string]CacheWindows, error) {
	defer rows.Close()
	out := map[string]CacheWindows{}
	var sums [6]sql.NullFloat64
	for rows.Next() {
		var key, provider, model string
		if keyCols == 1 {
			if err := rows.Scan(&provider, &model, &sums[0], &sums[1], &sums[2], &sums[3], &sums[4], &sums[5]); err != nil {
				return nil, err
			}
			key = provider + "/" + model
		} else {
			if err := rows.Scan(&key, &sums[0], &sums[1], &sums[2], &sums[3], &sums[4], &sums[5]); err != nil {
				return nil, err
			}
		}
		out[key] = CacheWindows{H1: cachePct(sums[0], sums[1]), H24: cachePct(sums[2], sums[3]), D7: cachePct(sums[4], sums[5])}
	}
	return out, rows.Err()
}

func cachePct(read, input sql.NullFloat64) *float64 {
	if !read.Valid || !input.Valid || input.Float64 <= 0 {
		return nil
	}
	p := read.Float64 / input.Float64 * 100
	return &p
}

// TargetResolutionHealth reports request outcomes for each attempted target,
// final resolutions from request_logs plus every fallback from
// request_attempts. The request_logs half preserves the historical
// virtual-only filter (final real-model resolutions are carried by the attempt
// rows and the real-model views); it is expressed against the denormalized
// route columns rather than a join to the control plane.
func (s *Scope) TargetResolutionHealth(ctx context.Context, c1, c24 string) (map[string]TargetHealth, error) {
	var out map[string]TargetHealth
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT key,
	max(success_1h), max(failure_1h), max(success_24h)
	FROM (
	SELECT l.resolved_provider||'/'||l.resolved_model AS key,
	CASE WHEN l.created_at >= ? AND l.http_status >= 200 AND l.http_status < 300 THEN 1 ELSE 0 END AS success_1h,
	CASE WHEN l.created_at >= ? AND NOT (l.http_status >= 200 AND l.http_status < 300) THEN 1 ELSE 0 END AS failure_1h,
	CASE WHEN l.http_status >= 200 AND l.http_status < 300 THEN 1 ELSE 0 END AS success_24h
	FROM request_logs l
	WHERE l.account_id=? AND l.created_at >= ? AND l.resolved_provider IS NOT NULL AND l.resolved_model IS NOT NULL
	AND (l.route_kind='virtual' OR (l.route_kind IS NULL AND l.route_status='legacy'))
	UNION ALL
	SELECT a.provider||'/'||a.model AS key,
	CASE WHEN a.created_at >= ? AND a.result='success' THEN 1 ELSE 0 END AS success_1h,
	CASE WHEN a.created_at >= ? AND a.result='failed'
	AND a.failure_class NOT IN ('client_cancelled','client_timeout') THEN 1 ELSE 0 END AS failure_1h,
	CASE WHEN a.result='success' THEN 1 ELSE 0 END AS success_24h
	FROM request_attempts a
	WHERE a.account_id=? AND a.created_at >= ?
	)
	GROUP BY key`, c1, c1, s.accountID, c24, c1, c1, s.accountID, c24)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = map[string]TargetHealth{}
		for rows.Next() {
			var id string
			var success1h, failure1h, success24h int
			if err := rows.Scan(&id, &success1h, &failure1h, &success24h); err != nil {
				return err
			}
			out[id] = TargetHealth{Success1h: success1h == 1, Failure1h: failure1h == 1, Success24h: success24h == 1}
		}
		return rows.Err()
	})
	return out, err
}
