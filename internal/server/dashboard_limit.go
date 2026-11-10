package server

import (
	"net/http"
	"strconv"
	"strings"
)

// Account request budgets.
//
// A logged-in account reaches the console API either with a cookie session or
// with an API token; both planes share one budget, so an account cannot double
// its allowance by switching credential. The budget exists for the expensive
// endpoints (the full model list, CSV exports, usage aggregates), which are
// otherwise free to call in a loop. It is deliberately far above anything a
// person does by using the UI: ordinary browsing, paging and dashboard polling
// never reach it, and the live activity stream is not counted at all -- it is
// bounded as a concurrency limit instead (windowLimiter.takeLive), so a tab left
// open all day costs nothing.
const (
	// dashboardRequestsPerMinute is the account's overall console budget per
	// minute: dashboard polling plus API tokens.
	dashboardRequestsPerMinute = 60
	// dashboardHeavyRequestsPerMinute is the tighter sub-budget for the endpoints
	// that read or stream a whole table in one response. They are the ones a
	// runaway client would pick, and a person uses each a handful of times.
	dashboardHeavyRequestsPerMinute = 10
	// dashboardLiveConcurrency bounds simultaneous live-stream connections per
	// account (one per open dashboard tab, in practice).
	dashboardLiveConcurrency = 4
)

// dashboardHeavyPrefixes are the console routes that read or stream a whole
// dataset in one response.
var dashboardHeavyPrefixes = []string{
	"/api/admin/backup/export",
	"/api/admin/usage",
	"/api/auth/account/export",
}

// isDashboardHeavy reports whether the route belongs to the tight sub-budget.
// The activity CSV exports all end in /export.
func isDashboardHeavy(r *http.Request) bool {
	for _, prefix := range dashboardHeavyPrefixes {
		if strings.HasPrefix(r.URL.Path, prefix) {
			return true
		}
	}
	return strings.HasPrefix(r.URL.Path, "/api/admin/") && strings.HasSuffix(r.URL.Path, "/activity/export")
}

// isDashboardLive reports whether the request opens the live activity stream.
func isDashboardLive(r *http.Request) bool { return r.URL.Path == "/api/admin/live" }

// chargeDashboardBudget applies the account's request budget to one console
// request and reports whether it may proceed, writing the refusal itself. The
// live stream is admitted and released around the handler instead of being
// counted, so an open tab does not spend the account's budget.
func (s *Server) chargeDashboardBudget(w http.ResponseWriter, r *http.Request, accountID string) bool {
	if accountID == "" {
		return true
	}
	if isDashboardLive(r) {
		key := "live\x00" + accountID
		quota := s.dashboardLiveLimiter.takeLive(key)
		setRateLimitHeaders(w, quota)
		if !quota.Allowed {
			w.Header().Set("Retry-After", "1")
			writeRateLimited(w, "Too many live connections for this account.")
			return false
		}
		defer s.dashboardLiveLimiter.releaseLive(key)
		return true
	}
	quota := s.dashboardLimiter.take(accountID)
	setRateLimitHeaders(w, quota)
	if !quota.Allowed {
		w.Header().Set("Retry-After", strconv.Itoa(quota.ResetSeconds))
		writeBudgetExceeded(w, quota, "Too many requests for this account. Wait for the window to reset.")
		return false
	}
	if !isDashboardHeavy(r) {
		return true
	}
	// Heavy routes draw on a second, much tighter budget, so one runaway client
	// cannot spend the account's whole allowance on table dumps alone.
	heavy := s.dashboardHeavyLimiter.take("heavy\x00" + accountID)
	if !heavy.Allowed {
		w.Header().Set("Retry-After", strconv.Itoa(heavy.ResetSeconds))
		writeBudgetExceeded(w, heavy, "This report was requested too often. Wait a moment and try again.")
		return false
	}
	return true
}

func setRateLimitHeaders(w http.ResponseWriter, q windowQuota) {
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(q.Limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(q.Remaining))
	h.Set("X-RateLimit-Reset", strconv.Itoa(q.ResetSeconds))
}

// writeBudgetExceeded answers a spent account budget. The body carries the
// quota numbers so a client can pace itself instead of retrying blind.
func writeBudgetExceeded(w http.ResponseWriter, quota windowQuota, message string) {
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
		"code":           "rate_limited",
		"message":        message,
		"limit":          quota.Limit,
		"window_seconds": 60,
		"retry_after":    quota.ResetSeconds,
	}})
}

func writeRateLimited(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
		"code":    "rate_limited",
		"message": message,
	}})
}
