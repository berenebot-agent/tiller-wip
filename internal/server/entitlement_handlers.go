package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/store"
)

// WS1 — entitlement / plan handlers. Own this file.
// Implements the endpoint contract in docs/stage_d_api_contract.md.

// writeLimitExceeded maps a store limit violation to the contract's 409
// response. It returns true when it handled the error, so create handlers can
// delegate the plan-cap branch and keep their own error cases.
func writeLimitExceeded(w http.ResponseWriter, err error) bool {
	var limit *store.LimitExceededError
	if !errors.As(err, &limit) {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": "limit_exceeded",
		"kind":  limit.Kind,
		"limit": limit.Limit,
	})
	return true
}

// writePlatformPlans lists the plan catalogue for the platform dashboard.
func (s *Server) writePlatformPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := s.storeHandle().ListPlans(r.Context())
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not list plans.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": plans})
}

// writePlatformPlanUpdate replaces the caps of one plan. Values below -1 are
// rejected; -1 is the unlimited sentinel.
func (s *Server) writePlatformPlanUpdate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		adminError(w, http.StatusNotFound, "plan_not_found", "Plan not found.")
		return
	}
	var input struct {
		MaxProviders          int `json:"max_providers"`
		MaxClientKeys         int `json:"max_client_keys"`
		MaxVirtualModels      int `json:"max_virtual_models"`
		MaxConcurrentStreams  int `json:"max_concurrent_streams"`
		ActivityRetentionDays int `json:"activity_retention_days"`
		MonthlyRequests       int `json:"monthly_requests"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	plan := store.Plan{
		Name:                  name,
		MaxProviders:          input.MaxProviders,
		MaxClientKeys:         input.MaxClientKeys,
		MaxVirtualModels:      input.MaxVirtualModels,
		MaxConcurrentStreams:  input.MaxConcurrentStreams,
		ActivityRetentionDays: input.ActivityRetentionDays,
		MonthlyRequests:       input.MonthlyRequests,
	}
	if err := s.storeHandle().UpdatePlan(r.Context(), plan); err != nil {
		switch {
		case errors.Is(err, store.ErrPlanNotFound):
			adminError(w, http.StatusNotFound, "plan_not_found", "Plan not found.")
		case errors.Is(err, store.ErrInvalidLimits):
			adminError(w, http.StatusBadRequest, "invalid_limits", "Plan limits must be -1 (unlimited) or greater.")
		default:
			adminError(w, http.StatusInternalServerError, "database_error", "Could not update the plan.")
		}
		return
	}
	s.recordPlatformAudit(r.Context(), store.AuditEvent{
		Event:      "platform.plan_changed",
		ActorType:  "platform",
		TargetType: "plan",
		TargetID:   name,
		Metadata:   map[string]string{"plan": name},
	})
	writeJSON(w, http.StatusOK, plan)
}

// writeAccountPlan returns the caller's plan caps and current usage counts.
func (s *Server) writeAccountPlan(w http.ResponseWriter, r *http.Request) {
	accountID, _ := r.Context().Value(accountKey).(string)
	if accountID == "" {
		adminError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	plan, err := s.storeHandle().EntitlementsForAccount(r.Context(), accountID)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load your plan.")
		return
	}
	sc := s.storeHandle().For(accountID)
	counts, err := sc.ResourceCounts(r.Context())
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load your usage.")
		return
	}
	now := time.Now()
	monthly, err := sc.UsageCount(r.Context(), store.UsagePeriod(now))
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load your usage.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"plan": plan.Name,
		"limits": map[string]any{
			"max_providers":           plan.MaxProviders,
			"max_client_keys":         plan.MaxClientKeys,
			"max_virtual_models":      plan.MaxVirtualModels,
			"max_concurrent_streams":  plan.MaxConcurrentStreams,
			"activity_retention_days": plan.ActivityRetentionDays,
			"monthly_requests":        plan.MonthlyRequests,
		},
		"usage": map[string]any{
			"providers":        counts.Providers,
			"client_keys":      counts.ClientKeys,
			"virtual_models":   counts.VirtualModels,
			"monthly_requests": monthly,
		},
		"period": store.UsagePeriod(now),
	})
}

// writeAccountPlanAssign moves an account onto a plan (operator action).
func (s *Server) writeAccountPlanAssign(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	if accountID == "" {
		adminError(w, http.StatusNotFound, "account_not_found", "Account not found.")
		return
	}
	var input struct {
		Plan string `json:"plan"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	plan := strings.TrimSpace(input.Plan)
	if plan == "" {
		adminError(w, http.StatusBadRequest, "invalid_request", "A plan name is required.")
		return
	}
	switch err := s.storeHandle().SetAccountPlan(r.Context(), accountID, plan); {
	case err == nil:
	case errors.Is(err, store.ErrPlanNotFound):
		adminError(w, http.StatusNotFound, "plan_not_found", "Plan not found.")
		return
	case errors.Is(err, sql.ErrNoRows):
		adminError(w, http.StatusNotFound, "account_not_found", "Account not found.")
		return
	default:
		adminError(w, http.StatusInternalServerError, "database_error", "Could not assign the plan.")
		return
	}
	s.recordPlatformAudit(r.Context(), store.AuditEvent{
		Event:      "platform.account_plan_changed",
		ActorType:  "platform",
		TargetType: "account",
		TargetID:   accountID,
		Metadata:   map[string]string{"plan": plan},
	})
	writeJSON(w, http.StatusOK, map[string]any{"account_id": accountID, "plan": plan})
}
