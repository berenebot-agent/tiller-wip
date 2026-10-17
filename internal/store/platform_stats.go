package store

import (
	"context"
	"time"
)

// PlatformUsageWindows contains aggregate request and token totals for one
// account. It deliberately exposes no tenant identifiers or row data.
type PlatformUsageWindows struct {
	Requests1h  int64 `json:"requests_1h"`
	Tokens1h    int64 `json:"tokens_1h"`
	Requests24h int64 `json:"requests_24h"`
	Tokens24h   int64 `json:"tokens_24h"`
	Requests7d  int64 `json:"requests_7d"`
	Tokens7d    int64 `json:"tokens_7d"`
}

// platformUsageSelect is the shared aggregate column list. Its first four bind
// parameters are the select-list cutoffs in the order platformUsageCuts
// returns them.
const platformUsageSelect = `coalesce(sum(CASE WHEN created_at >= ? THEN 1 ELSE 0 END),0),
	coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0)+coalesce(output_tokens,0) ELSE 0 END),0),
	coalesce(sum(CASE WHEN created_at >= ? THEN 1 ELSE 0 END),0),
	coalesce(sum(CASE WHEN created_at >= ? THEN coalesce(input_tokens,0)+coalesce(output_tokens,0) ELSE 0 END),0),
	count(*),
	coalesce(sum(coalesce(input_tokens,0)+coalesce(output_tokens,0)),0)`

// platformUsageCuts returns the bind parameters shared by the usage queries:
// the four select-list cutoffs (1h, 1h, 24h, 24h) then the 7-day cutoff the
// WHERE clause uses.
func platformUsageCuts(current time.Time) []string {
	hour := current.UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	day := current.UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	week := current.UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)
	return []string{hour, hour, day, day, week}
}

// PlatformUsageForAccounts returns the Activity usage windows for several
// accounts in one grouped query, so a platform page costs a fixed number of
// queries instead of one per account. Accounts with no rows in the window are
// absent from the map rather than present as zeroes.
func (s *Store) PlatformUsageForAccounts(ctx context.Context, accountIDs []string, current time.Time) (map[string]PlatformUsageWindows, error) {
	out := make(map[string]PlatformUsageWindows, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	if s.activity == nil {
		return nil, ErrActivityUnavailable
	}
	cuts := platformUsageCuts(current)
	placeholders, idArgs := inPlaceholders(accountIDs)
	args := make([]any, 0, len(cuts)+len(idArgs))
	for _, cut := range cuts[:4] {
		args = append(args, cut)
	}
	for _, id := range idArgs {
		args = append(args, id)
	}
	args = append(args, cuts[4])
	rows, err := s.activity.QueryContext(ctx, `SELECT account_id,`+platformUsageSelect+`
		FROM request_logs WHERE account_id IN (`+placeholders+`) AND created_at >= ?
		GROUP BY account_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID string
		var usage PlatformUsageWindows
		if err := rows.Scan(&accountID, &usage.Requests1h, &usage.Tokens1h, &usage.Requests24h, &usage.Tokens24h, &usage.Requests7d, &usage.Tokens7d); err != nil {
			return nil, err
		}
		out[accountID] = usage
	}
	return out, rows.Err()
}

// PlatformUsageTotals returns the same windows summed across every account in
// one query. It replaces an account-by-account loop that grew with the number
// of accounts on the platform. It is platform-admin only.
func (s *Store) PlatformUsageTotals(ctx context.Context, current time.Time) (PlatformUsageWindows, error) {
	var usage PlatformUsageWindows
	if s.activity == nil {
		return usage, ErrActivityUnavailable
	}
	cuts := platformUsageCuts(current)
	args := make([]any, 0, len(cuts))
	for _, cut := range cuts {
		args = append(args, cut)
	}
	err := s.activity.QueryRowContext(ctx, `SELECT `+platformUsageSelect+`
		FROM request_logs WHERE created_at >= ?`, args...).Scan(
		&usage.Requests1h, &usage.Tokens1h, &usage.Requests24h, &usage.Tokens24h, &usage.Requests7d, &usage.Tokens7d)
	if err != nil {
		return PlatformUsageWindows{}, err
	}
	return usage, nil
}
