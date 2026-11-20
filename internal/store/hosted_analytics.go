package store

import (
	"context"
	"time"
)

const hostedAnalyticsRetentionDays = 90

type HostedAnalyticsDay struct {
	Day          string `json:"day"`
	AppLoads     int64  `json:"app_loads"`
	PrivacyLoads int64  `json:"privacy_loads"`
	TermsLoads   int64  `json:"terms_loads"`
	Signups      int64  `json:"signups"`
}

func (s *Store) IncrementHostedAnalytics(ctx context.Context, now time.Time, metric string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO hosted_analytics_daily(day, metric, total) VALUES (?, ?, 1)
		ON CONFLICT(day, metric) DO UPDATE SET total = total + 1`, now.UTC().Format(time.DateOnly), metric)
	return err
}

func (s *Store) HostedAnalyticsDays(ctx context.Context, now time.Time) ([]HostedAnalyticsDay, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day,
		sum(CASE WHEN metric = 'app_loads' THEN total ELSE 0 END),
		sum(CASE WHEN metric = 'privacy_loads' THEN total ELSE 0 END),
		sum(CASE WHEN metric = 'terms_loads' THEN total ELSE 0 END),
		sum(CASE WHEN metric = 'signups' THEN total ELSE 0 END)
		FROM hosted_analytics_daily WHERE day BETWEEN ? AND ? GROUP BY day ORDER BY day DESC`,
		hostedAnalyticsFirstDay(now), now.UTC().Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	days := []HostedAnalyticsDay{}
	for rows.Next() {
		var day HostedAnalyticsDay
		if err := rows.Scan(&day.Day, &day.AppLoads, &day.PrivacyLoads, &day.TermsLoads, &day.Signups); err != nil {
			return nil, err
		}
		days = append(days, day)
	}
	return days, rows.Err()
}

func (s *Store) PruneHostedAnalytics(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM hosted_analytics_daily WHERE day < ?`, hostedAnalyticsFirstDay(now))
	return err
}

func hostedAnalyticsFirstDay(now time.Time) string {
	return now.UTC().AddDate(0, 0, 1-hostedAnalyticsRetentionDays).Format(time.DateOnly)
}
