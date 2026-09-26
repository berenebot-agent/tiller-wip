package store

import (
	"context"
	"time"
)

// PlatformUsageWindows contains aggregate request and token totals for one
// account. It deliberately exposes no tenant identifiers or row data.
type PlatformUsageWindows struct {
	Requests24h int64 `json:"requests_24h"`
	Tokens24h   int64 `json:"tokens_24h"`
	Requests7d  int64 `json:"requests_7d"`
	Tokens7d    int64 `json:"tokens_7d"`
}

// PlatformUsage returns aggregate counts scoped to this account. The platform
// admin handler sums these account-level aggregates without querying tenant
// rows across account boundaries.
func (s *Scope) PlatformUsage(ctx context.Context, current time.Time) (PlatformUsageWindows, error) {
	var usage PlatformUsageWindows
	if err := s.withActivity(ctx, func(q querier) error {
		cut24h := current.UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
		cut7d := current.UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)
		return q.QueryRowContext(ctx, `SELECT
			coalesce(sum(CASE WHEN created_at >= ? THEN 1 ELSE 0 END),0),
			coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0)+coalesce(output_tokens,0) ELSE 0 END),0),
			count(*),
			coalesce(sum(coalesce(input_tokens,0)+coalesce(output_tokens,0)),0)
			FROM request_logs WHERE account_id=? AND created_at >= ?`, cut24h, cut24h, s.accountID, cut7d).Scan(
			&usage.Requests24h, &usage.Tokens24h, &usage.Requests7d, &usage.Tokens7d)
	}); err != nil {
		return PlatformUsageWindows{}, err
	}
	return usage, nil
}
