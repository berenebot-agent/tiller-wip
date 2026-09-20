package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// UsagePeriod formats a time as the UTC 'YYYY-MM' counter key. A new month is a
// new row at zero, so rollover needs no reset job.
func UsagePeriod(t time.Time) string {
	return t.UTC().Format("2006-01")
}

// IncrementUsageCounter bumps the account's routed-request counter for the
// period. It is best-effort and account-scoped: a failed increment under-counts
// slightly rather than blocking the request.
func (s *Scope) IncrementUsageCounter(ctx context.Context, period string) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO usage_counters(account_id,period,requests,updated_at) VALUES(?,?,1,?)
ON CONFLICT(account_id,period) DO UPDATE SET requests=requests+1,updated_at=excluded.updated_at`, s.accountID, period, now())
	return err
}

// UsageCount returns the routed-request count for the account and period, or 0
// when no counter row exists yet.
func (s *Scope) UsageCount(ctx context.Context, period string) (int, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `SELECT requests FROM usage_counters WHERE account_id=? AND period=?`, s.accountID, period).Scan(&n)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}

// NextPeriodStart returns the UTC instant the given period rolls over (the
// first moment of the following month). It is used for the Retry-After hint on
// a monthly-quota rejection.
func NextPeriodStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}
