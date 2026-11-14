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
// person does by using the UI and the live activity stream is not counted at
// all -- it is bounded as a concurrency limit instead (windowLimiter.takeLive),
// so a tab left open all day costs nothing. The identity probes are exempt
// (dashboardSessionProbes): polling and navigation must never be able to spend
// the budget to the point that the console decides the user is signed out.
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

// dashboardSessionProbes are the two identity endpoints the console calls on
// every page load to decide whether the viewer is signed in. They are exempt
// from the account budget on purpose.
//
// The budget is charged at the principal (requireAdmin/requireUser), which
// correctly puts previously-unmetered routes under it, but that also swept in
// the session probe. A spent budget then answered GET /api/admin/session with
// 429, and the SPA reads any non-200 probe as "signed out": it paints the login
// card and the operator is locked out of their own console until the window
// rolls over. An ordinary dashboard tab reaches 60 requests/minute between
// polling and navigation, so this was reachable in normal use, not just under
// load. A spent budget is a reason to slow down, never to deny that the session
// exists. These paths are cheap single-row reads, so exempting them costs the
// budget nothing.
var dashboardSessionProbes = map[string]bool{
	"/api/admin/session": true,
	"/api/auth/session":  true,
}

// isDashboardSessionProbe reports whether the route is an identity probe that
// must answer even when the account budget is spent.
func isDashboardSessionProbe(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return dashboardSessionProbes[r.URL.Path]
}

// chargeDashboardBudget applies the account's request budget to one console
// request and reports whether it may proceed, writing the refusal itself. The
// live stream is admitted and released around the handler instead of being
// counted, so an open tab does not spend the account's budget.
func (s *Server) chargeDashboardBudget(w http.ResponseWriter, r *http.Request, accountID string) bool {
	if accountID == "" {
		return true
	}
	// An identity probe must answer even with the budget spent: refusing it makes
	// the console look signed out. See dashboardSessionProbes.
	if isDashboardSessionProbe(r) {
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
