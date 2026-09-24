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
// slightly rather than blocking the request. It is used for the unlimited path,
// where the counter is display-only; the capped path uses ReserveUsageCounter.
func (s *Scope) IncrementUsageCounter(ctx context.Context, period string) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO usage_counters(account_id,period,requests,updated_at) VALUES(?,?,1,?)
ON CONFLICT(account_id,period) DO UPDATE SET requests=requests+1,updated_at=excluded.updated_at`, s.accountID, period, now())
	return err
}

// ReserveUsageCounter atomically reserves one monthly request slot, returning
// false when the account is already at limit. The check and the increment run in
// one transaction against the core database, so concurrent requests cannot all
// observe the same pre-burst count the way a separate read-then-increment would;
// a rejected request performs no increment. A limit of Unlimited always
// reserves. Callers invoke it before any upstream work so a rejected request
// never reaches a provider, and it is independent of Activity logging.
func (s *Scope) ReserveUsageCounter(ctx context.Context, period string, limit int) (bool, error) {
	if limit == Unlimited {
		return true, nil
	}
	if limit < 0 {
		return false, nil
	}
	var reserved bool
	err := s.RunTx(ctx, nil, func(tx *Scope) error {
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO usage_counters(account_id,period,requests,updated_at) VALUES(?,?,0,?) ON CONFLICT(account_id,period) DO NOTHING`, tx.accountID, period, now()); err != nil {
			return err
		}
		res, err := tx.q.ExecContext(ctx, `UPDATE usage_counters SET requests=requests+1,updated_at=? WHERE account_id=? AND period=? AND requests < ?`, now(), tx.accountID, period, limit)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		reserved = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return reserved, nil
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
