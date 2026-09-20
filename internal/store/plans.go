package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Unlimited is the sentinel value for an entitlement with no cap. A plan column
// of -1 means the account is not limited on that dimension.
const Unlimited = -1

// ErrPlanNotFound is returned when a plan name does not exist.
var ErrPlanNotFound = errors.New("store: plan not found")

// ErrLimitExceeded reports that an account is at its plan's cap for a resource.
// Kind is one of "providers", "client_keys" or "virtual_models"; Limit is the
// plan cap. Handlers map it to a 409 with a stable payload.
type LimitExceededError struct {
	Kind  string
	Limit int
}

func (e *LimitExceededError) Error() string {
	return fmt.Sprintf("store: plan limit reached for %s (limit %d)", e.Kind, e.Limit)
}

// Plan is one row of the plans catalogue. A Limit value of Unlimited means the
// dimension is uncapped.
type Plan struct {
	Name                 string `json:"name"`
	MaxProviders         int    `json:"max_providers"`
	MaxClientKeys        int    `json:"max_client_keys"`
	MaxVirtualModels     int    `json:"max_virtual_models"`
	MaxConcurrentStreams int    `json:"max_concurrent_streams"`
	ActivityRetentionDays int   `json:"activity_retention_days"`
	MonthlyRequests      int    `json:"monthly_requests"`
	UpdatedAt            string `json:"updated_at"`
}

// EntitlementsForAccount resolves the plan an account is on. The local account
// is seeded on the free plan, but local mode never enforces limits (the server
// gates enforcement on hosted mode).
func (s *Store) EntitlementsForAccount(ctx context.Context, accountID string) (Plan, error) {
	var p Plan
	err := s.db.QueryRowContext(ctx, `SELECT p.name,p.max_providers,p.max_client_keys,p.max_virtual_models,p.max_concurrent_streams,p.activity_retention_days,p.monthly_requests,p.updated_at FROM accounts a JOIN plans p ON p.name=a.plan WHERE a.id=?`, accountID).
		Scan(&p.Name, &p.MaxProviders, &p.MaxClientKeys, &p.MaxVirtualModels, &p.MaxConcurrentStreams, &p.ActivityRetentionDays, &p.MonthlyRequests, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrPlanNotFound
	}
	return p, err
}

// ListPlans returns the plan catalogue ordered by name.
func (s *Store) ListPlans(ctx context.Context) ([]Plan, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,max_providers,max_client_keys,max_virtual_models,max_concurrent_streams,activity_retention_days,monthly_requests,updated_at FROM plans ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		var p Plan
		if err := rows.Scan(&p.Name, &p.MaxProviders, &p.MaxClientKeys, &p.MaxVirtualModels, &p.MaxConcurrentStreams, &p.ActivityRetentionDays, &p.MonthlyRequests, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePlan replaces the caps for an existing plan. Negative values below -1
// are rejected; -1 is the unlimited sentinel.
func (s *Store) UpdatePlan(ctx context.Context, p Plan) error {
	for _, v := range []int{p.MaxProviders, p.MaxClientKeys, p.MaxVirtualModels, p.MaxConcurrentStreams, p.ActivityRetentionDays, p.MonthlyRequests} {
		if v < Unlimited {
			return errors.New("store: plan limits must be >= -1")
		}
	}
	res, err := s.db.ExecContext(ctx, `UPDATE plans SET max_providers=?,max_client_keys=?,max_virtual_models=?,max_concurrent_streams=?,activity_retention_days=?,monthly_requests=?,updated_at=? WHERE name=?`,
		p.MaxProviders, p.MaxClientKeys, p.MaxVirtualModels, p.MaxConcurrentStreams, p.ActivityRetentionDays, p.MonthlyRequests, now(), p.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPlanNotFound
	}
	return nil
}

// SetAccountPlan moves an account onto an existing plan.
func (s *Store) SetAccountPlan(ctx context.Context, accountID, plan string) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM plans WHERE name=?`, plan).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrPlanNotFound
	}
	res, err := s.db.ExecContext(ctx, `UPDATE accounts SET plan=?,updated_at=? WHERE id=?`, plan, now(), accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// checkCreateLimit counts rows of a tenant resource for this scope and returns
// ErrLimitExceeded when the plan cap is reached. It runs inside the caller's
// create transaction so the count and insert are atomic (no TOCTOU race). A
// cap of Unlimited is a no-op. table and kind are internal constants, never
// request input, so the interpolation is safe.
func (s *Scope) checkCreateLimit(ctx context.Context, table, kind string, limit int) error {
	if limit == Unlimited {
		return nil
	}
	var count int
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE account_id=?`, s.accountID).Scan(&count); err != nil {
		return err
	}
	if count >= limit {
		return &LimitExceededError{Kind: kind, Limit: limit}
	}
	return nil
}

// EnforceProviderLimit, EnforceClientKeyLimit and EnforceVirtualModelLimit are
// the plan-aware guards the create paths call inside their transaction. Each
// resolves the account's plan and checks the matching cap. They are no-ops in
// local mode because the caller does not invoke them there.
func (s *Scope) EnforceProviderLimit(ctx context.Context) error {
	plan, err := s.planForAccount(ctx)
	if err != nil {
		return err
	}
	return s.checkCreateLimit(ctx, "providers", "providers", plan.MaxProviders)
}

func (s *Scope) EnforceClientKeyLimit(ctx context.Context) error {
	plan, err := s.planForAccount(ctx)
	if err != nil {
		return err
	}
	return s.checkCreateLimit(ctx, "client_keys", "client_keys", plan.MaxClientKeys)
}

func (s *Scope) EnforceVirtualModelLimit(ctx context.Context) error {
	plan, err := s.planForAccount(ctx)
	if err != nil {
		return err
	}
	return s.checkCreateLimit(ctx, "virtual_models", "virtual_models", plan.MaxVirtualModels)
}

// planForAccount resolves the plan for the scope's account inside a
// transaction. A missing plan row is not fatal for the create path: it falls
// back to unlimited so a misconfigured catalogue can never lock an account out
// of creating resources. The operator UI surfaces a missing plan separately.
func (s *Scope) planForAccount(ctx context.Context) (Plan, error) {
	var p Plan
	err := s.q.QueryRowContext(ctx, `SELECT max_providers,max_client_keys,max_virtual_models FROM plans WHERE name=(SELECT plan FROM accounts WHERE id=?)`, s.accountID).
		Scan(&p.MaxProviders, &p.MaxClientKeys, &p.MaxVirtualModels)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{MaxProviders: Unlimited, MaxClientKeys: Unlimited, MaxVirtualModels: Unlimited}, nil
	}
	return p, err
}

// EffectiveRetentionDays clamps a configured client-key retention to the plan's
// maximum. A plan cap of Unlimited leaves the configured value unchanged. The
// clamp is applied at enforcement time only; stored client rows are never
// rewritten, so raising a plan restores previously clamped history.
func EffectiveRetentionDays(configured, planMax int) int {
	if planMax == Unlimited || planMax <= 0 {
		return configured
	}
	if configured <= 0 {
		return planMax
	}
	if configured < planMax {
		return configured
	}
	return planMax
}
