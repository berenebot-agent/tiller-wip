package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Unlimited is the sentinel value for an entitlement with no cap. A plan column
// of -1 means the account is not limited on that dimension.
const Unlimited = -1

// DefaultPlan is the plan every account starts on. It is referenced by
// accounts.plan's default and can never be deleted.
const DefaultPlan = "free"

// ErrPlanNotFound is returned when a plan name does not exist.
var ErrPlanNotFound = errors.New("store: plan not found")

// ErrInvalidLimits is returned when a plan cap is below the Unlimited sentinel
// (-1). Handlers map it to a 400 invalid_limits.
var ErrInvalidLimits = errors.New("store: plan limits must be >= -1")

// ErrInvalidPlanName is returned when a plan name is not a lowercase slug. The
// name is used in request paths and as accounts.plan, so it is constrained.
var ErrInvalidPlanName = errors.New("store: plan name must be a lowercase slug")

// ErrPlanExists is returned when creating a plan whose name is already taken.
var ErrPlanExists = errors.New("store: plan name already exists")

// ErrPlanReserved is returned when renaming or deleting the default plan. The
// default is hardcoded as the account-creation fallback, so it must always
// exist under its own name.
var ErrPlanReserved = errors.New("store: the default plan is reserved")

// planNamePattern keeps plan identifiers URL-safe and predictable. It is the
// same shape used for provider/model slugs elsewhere.
var planNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// PlanInUseError reports that accounts still reference a plan, so it cannot be
// deleted. Accounts counts the references at the time of the check.
type PlanInUseError struct {
	Plan     string
	Accounts int
}

func (e *PlanInUseError) Error() string {
	return fmt.Sprintf("store: plan %q is still assigned to %d account(s)", e.Plan, e.Accounts)
}

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
	Name                  string `json:"name"`
	MaxProviders          int    `json:"max_providers"`
	MaxClientKeys         int    `json:"max_client_keys"`
	MaxVirtualModels      int    `json:"max_virtual_models"`
	MaxConcurrentStreams  int    `json:"max_concurrent_streams"`
	ActivityRetentionDays int    `json:"activity_retention_days"`
	MonthlyRequests       int    `json:"monthly_requests"`
	UpdatedAt             string `json:"updated_at"`
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

// validatePlanLimits rejects caps below the Unlimited sentinel. -1 means
// unlimited; every other value must be >= 0.
func validatePlanLimits(p Plan) error {
	for _, v := range []int{p.MaxProviders, p.MaxClientKeys, p.MaxVirtualModels, p.MaxConcurrentStreams, p.ActivityRetentionDays, p.MonthlyRequests} {
		if v < Unlimited {
			return ErrInvalidLimits
		}
	}
	return nil
}

// validPlanName reports whether name is a lowercase slug.
func validPlanName(name string) bool { return planNamePattern.MatchString(name) }

// CreatePlan inserts a new plan into the catalogue. The name must be a
// lowercase slug and caps must be >= -1; a taken name returns ErrPlanExists.
func (s *Store) CreatePlan(ctx context.Context, p Plan) error {
	if !validPlanName(p.Name) {
		return ErrInvalidPlanName
	}
	if err := validatePlanLimits(p); err != nil {
		return err
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM plans WHERE name=?`, p.Name).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return ErrPlanExists
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO plans(name,max_providers,max_client_keys,max_virtual_models,max_concurrent_streams,activity_retention_days,monthly_requests,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		p.Name, p.MaxProviders, p.MaxClientKeys, p.MaxVirtualModels, p.MaxConcurrentStreams, p.ActivityRetentionDays, p.MonthlyRequests, now()); err != nil {
		// The pre-check narrows but cannot close the race; the PK is the backstop.
		if isPlanNameConflict(err) {
			return ErrPlanExists
		}
		return err
	}
	return nil
}

// isPlanNameConflict reports whether err is a SQLite unique-constraint failure
// on plans.name. The pre-checks make this only reachable under a concurrent
// create; it exists so that race maps to the same typed error.
func isPlanNameConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: plans.name")
}

// UpdatePlan replaces the caps for an existing plan. Negative values below -1
// are rejected; -1 is the unlimited sentinel. The plan name is not changed; use
// RenamePlan for that.
func (s *Store) UpdatePlan(ctx context.Context, p Plan) error {
	if err := validatePlanLimits(p); err != nil {
		return err
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

// RenamePlan changes a plan's name and caps in one transaction, preserving
// every account's assignment. accounts.plan has a RESTRICT foreign key with no
// ON UPDATE action, so the row is copied to the new name, the accounts are
// repointed, and only then is the old row deleted. A taken new name returns
// ErrPlanExists; the name must be a lowercase slug.
func (s *Store) RenamePlan(ctx context.Context, oldName string, p Plan) error {
	if oldName == DefaultPlan {
		return ErrPlanReserved
	}
	if !validPlanName(p.Name) {
		return ErrInvalidPlanName
	}
	if err := validatePlanLimits(p); err != nil {
		return err
	}
	if p.Name == oldName {
		return s.UpdatePlan(ctx, p)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM plans WHERE name=?`, oldName).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrPlanNotFound
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM plans WHERE name=?`, p.Name).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return ErrPlanExists
	}
	ts := now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plans(name,max_providers,max_client_keys,max_virtual_models,max_concurrent_streams,activity_retention_days,monthly_requests,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		p.Name, p.MaxProviders, p.MaxClientKeys, p.MaxVirtualModels, p.MaxConcurrentStreams, p.ActivityRetentionDays, p.MonthlyRequests, ts); err != nil {
		if isPlanNameConflict(err) {
			return ErrPlanExists
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET plan=?,updated_at=? WHERE plan=?`, p.Name, ts, oldName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plans WHERE name=?`, oldName); err != nil {
		return err
	}
	return tx.Commit()
}

// DeletePlan removes a plan from the catalogue. The default plan is reserved
// and a plan with accounts still assigned returns PlanInUseError. The
// accounts.plan foreign key is the non-bypassable backstop.
func (s *Store) DeletePlan(ctx context.Context, name string) error {
	if name == "" {
		return ErrPlanNotFound
	}
	if name == DefaultPlan {
		return ErrPlanReserved
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM plans WHERE name=?`, name).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrPlanNotFound
	}
	var inUse int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE plan=?`, name).Scan(&inUse); err != nil {
		return err
	}
	if inUse > 0 {
		return &PlanInUseError{Plan: name, Accounts: inUse}
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM plans WHERE name=?`, name)
	return err
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

// AccountResourceCounts is the current count of the capped tenant resources for
// one account, as surfaced by GET /api/auth/account/plan.
type AccountResourceCounts struct {
	Providers     int `json:"providers"`
	ClientKeys    int `json:"client_keys"`
	VirtualModels int `json:"virtual_models"`
}

// ResourceCounts returns the account's current provider, client-key and
// virtual-model counts. All three are read in one statement so the usage panel
// is internally consistent.
func (s *Scope) ResourceCounts(ctx context.Context) (AccountResourceCounts, error) {
	var c AccountResourceCounts
	err := s.q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM providers WHERE account_id=?), (SELECT count(*) FROM client_keys WHERE account_id=?), (SELECT count(*) FROM virtual_models WHERE account_id=?)`, s.accountID, s.accountID, s.accountID).
		Scan(&c.Providers, &c.ClientKeys, &c.VirtualModels)
	return c, err
}
