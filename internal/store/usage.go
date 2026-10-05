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

// CostWindows holds estimated spend in micro-dollars (1e-6 USD) for the three
// lookback windows. A value is nil when the group had no costed requests.
// These are display estimates from published model prices, or exact
// provider-reported costs where the provider supplied one — never a billing
// figure. See docs/roadmap_usage_cost_quota.md.
type CostWindows struct {
	H1        *int64       `json:"1h"`
	H24       *int64       `json:"24h"`
	D7        *int64       `json:"7d"`
	Estimated UsageWindows `json:"estimated"`
}

// usageTypeWindows holds per-type token sums (input/output/cache-read/
// cache-creation) for the three lookback windows. A group with no rows in a
// window has zeroed totals.
type usageTypeWindows struct {
	InputH1, InputH24, InputD7                int64
	OutputH1, OutputH24, OutputD7             int64
	CacheReadH1, CacheReadH24, CacheReadD7    int64
	CacheWriteH1, CacheWriteH24, CacheWriteD7 int64
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
const virtualAttributionFilter = `route_kind='virtual' AND route_model IS NOT NULL`

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

// costSelect sums cost per window, preferring the provider-reported exact cost
// (provider_cost_micros) over the models.dev-derived estimate
// (estimated_cost_micros) when both are present. coalesce evaluates the first
// non-null argument, so the exact value wins per row. Only rows with a non-null
// cost contribute; a window with no costed rows yields 0.
const costSelect = `sum(CASE WHEN created_at >= ? THEN coalesce(provider_cost_micros, estimated_cost_micros) END),
sum(CASE WHEN created_at >= ? THEN coalesce(provider_cost_micros, estimated_cost_micros) END),
sum(CASE WHEN created_at >= ? THEN coalesce(provider_cost_micros, estimated_cost_micros) END),
sum(CASE WHEN provider_cost_micros IS NULL AND estimated_cost_micros IS NOT NULL THEN 1 ELSE 0 END)`

// EstimatedClients returns the client key IDs that have at least one row whose
// input tokens are a local estimate (the provider omitted usage) within the
// widest window. The UI marks these so an estimated total is never mistaken
// for provider-reported accounting.
func (s *Scope) EstimatedClients(ctx context.Context, c7 string) (map[string]bool, error) {
	out := map[string]bool{}
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT DISTINCT client_key_id FROM request_logs WHERE account_id=? AND created_at >= ? AND input_tokens_estimated=1`, s.accountID, c7)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out[id] = true
		}
		return rows.Err()
	})
	return out, err
}

// scanCost scans the windowed cost sums. keyCols is the number of leading
// group-by columns (0 for client/virtual, 1 for provider+model). A window that
// summed to zero is reported as nil (no costed activity) rather than $0.
func scanCost(rows *sql.Rows, keyCols int) (map[string]CostWindows, error) {
	defer rows.Close()
	out := map[string]CostWindows{}
	for rows.Next() {
		var key, provider, model string
		var w CostWindows
		var h1, h24, d7 sql.NullInt64
		var estimates int64
		if keyCols == 1 {
			if err := rows.Scan(&provider, &model, &h1, &h24, &d7, &estimates); err != nil {
				return nil, err
			}
			key = provider + "/" + model
		} else {
			if err := rows.Scan(&key, &h1, &h24, &d7, &estimates); err != nil {
				return nil, err
			}
		}
		if h1.Valid {
			w.H1 = &h1.Int64
		}
		w.Estimated = UsageWindows{H1: estimates, H24: estimates, D7: estimates}
		if h24.Valid {
			w.H24 = &h24.Int64
		}
		if d7.Valid {
			w.D7 = &d7.Int64
		}
		out[key] = w
	}
	return out, rows.Err()
}

// CostByClient returns estimated spend per client key for the three windows.
func (s *Scope) CostByClient(ctx context.Context, c1, c24, c7 string) (map[string]CostWindows, error) {
	var out map[string]CostWindows
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT client_key_id, `+costSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? GROUP BY client_key_id`, c1, c24, c7, s.accountID, c7)
		if err != nil {
			return err
		}
		out, err = scanCost(rows, 0)
		return err
	})
	return out, err
}

// CostByVirtual returns estimated spend per virtual model for the three windows.
func (s *Scope) CostByVirtual(ctx context.Context, c1, c24, c7 string) (map[string]CostWindows, error) {
	var out map[string]CostWindows
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT route_model, `+costSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? AND `+virtualAttributionFilter+` GROUP BY route_model`, c1, c24, c7, s.accountID, c7)
		if err != nil {
			return err
		}
		out, err = scanCost(rows, 0)
		return err
	})
	return out, err
}

// CostByReal returns estimated spend per real provider/model for the three
// windows.
func (s *Scope) CostByReal(ctx context.Context, c1, c24, c7 string) (map[string]CostWindows, error) {
	var out map[string]CostWindows
	err := s.withActivity(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT resolved_provider, resolved_model, `+costSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? AND resolved_provider IS NOT NULL GROUP BY resolved_provider, resolved_model`, c1, c24, c7, s.accountID, c7)
		if err != nil {
			return err
		}
		out, err = scanCost(rows, 1)
		return err
	})
	return out, err
}

// tokenTypeSelect sums each token type per window (1h/24h/7d), in the order
// input, output, cache-read, cache-creation, each for the three windows. The
// argument order is c1,c24,c7 repeated for each of the four types.
const tokenTypeSelect = `coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(output_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(output_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(output_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(cache_read_input_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(cache_read_input_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(cache_read_input_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(cache_creation_input_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(cache_creation_input_tokens,0) ELSE 0 END),0),
coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(cache_creation_input_tokens,0) ELSE 0 END),0)`

// TokenTypeWindows is the per-type token breakdown for the three lookback
// windows. It is the token detail behind the single total UsageWindows sum.
type TokenTypeWindows struct {
	Input       UsageWindows `json:"input"`
	Output      UsageWindows `json:"output"`
	CacheRead   UsageWindows `json:"cache_read"`
	CacheCreate UsageWindows `json:"cache_creation"`
}

func tokenTypeArgs(c1, c24, c7 string) []any {
	args := make([]any, 0, 12)
	for i := 0; i < 4; i++ {
		args = append(args, c1, c24, c7)
	}
	return args
}

func scanTokenTypes(rows *sql.Rows, keyCols int) (map[string]TokenTypeWindows, error) {
	defer rows.Close()
	out := map[string]TokenTypeWindows{}
	for rows.Next() {
		var key, provider, model string
		var vals [12]int64
		dest := make([]any, 0, keyCols+12)
		if keyCols == 1 {
			dest = append(dest, &provider, &model)
		} else {
			dest = append(dest, &key)
		}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if keyCols == 1 {
			key = provider + "/" + model
		}
		out[key] = TokenTypeWindows{
			Input:       UsageWindows{H1: vals[0], H24: vals[1], D7: vals[2]},
			Output:      UsageWindows{H1: vals[3], H24: vals[4], D7: vals[5]},
			CacheRead:   UsageWindows{H1: vals[6], H24: vals[7], D7: vals[8]},
			CacheCreate: UsageWindows{H1: vals[9], H24: vals[10], D7: vals[11]},
		}
	}
	return out, rows.Err()
}

// TokenTypesByClient returns the per-type token breakdown per client key.
func (s *Scope) TokenTypesByClient(ctx context.Context, c1, c24, c7 string) (map[string]TokenTypeWindows, error) {
	var out map[string]TokenTypeWindows
	err := s.withActivity(ctx, func(q querier) error {
		args := append([]any{}, tokenTypeArgs(c1, c24, c7)...)
		args = append(args, s.accountID, c7)
		rows, err := q.QueryContext(ctx, `SELECT client_key_id, `+tokenTypeSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? GROUP BY client_key_id`, args...)
		if err != nil {
			return err
		}
		out, err = scanTokenTypes(rows, 0)
		return err
	})
	return out, err
}

// TokenTypesByVirtual returns the per-type token breakdown per virtual model.
func (s *Scope) TokenTypesByVirtual(ctx context.Context, c1, c24, c7 string) (map[string]TokenTypeWindows, error) {
	var out map[string]TokenTypeWindows
	err := s.withActivity(ctx, func(q querier) error {
		args := append([]any{}, tokenTypeArgs(c1, c24, c7)...)
		args = append(args, s.accountID, c7)
		rows, err := q.QueryContext(ctx, `SELECT route_model, `+tokenTypeSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? AND `+virtualAttributionFilter+` GROUP BY route_model`, args...)
		if err != nil {
			return err
		}
		out, err = scanTokenTypes(rows, 0)
		return err
	})
	return out, err
}

// TokenTypesByReal returns the per-type token breakdown per real model.
func (s *Scope) TokenTypesByReal(ctx context.Context, c1, c24, c7 string) (map[string]TokenTypeWindows, error) {
	var out map[string]TokenTypeWindows
	err := s.withActivity(ctx, func(q querier) error {
		args := append([]any{}, tokenTypeArgs(c1, c24, c7)...)
		args = append(args, s.accountID, c7)
		rows, err := q.QueryContext(ctx, `SELECT resolved_provider, resolved_model, `+tokenTypeSelect+` FROM request_logs WHERE account_id=? AND created_at >= ? AND resolved_provider IS NOT NULL GROUP BY resolved_provider, resolved_model`, args...)
		if err != nil {
			return err
		}
		out, err = scanTokenTypes(rows, 1)
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
	AND l.route_kind='virtual'
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
