package server

import (
	"net/http"
	"testing"
	"time"
)

// TestDashboardBudgetDoesNotAffectNormalUse proves the ordinary console traffic
// -- reading the account, listing keys, paging -- never spends enough of the
// per-account budget to be refused, and that the response advertises the quota.
func TestDashboardBudgetDoesNotAffectNormalUse(t *testing.T) {
	t.Parallel()
	_, api, _ := hostedAccountServer(t)
	for i := 0; i < dashboardHeavyRequestsPerMinute; i++ {
		status, _, header := api.request("GET", "/api/admin/client-keys?limit=50", nil)
		if status != http.StatusOK {
			t.Fatalf("ordinary request %d = %d, want 200", i, status)
		}
		if header.Get("X-RateLimit-Limit") == "" {
			t.Fatal("console responses should advertise the account budget")
		}
	}
}

// TestDashboardBudgetChargesCookieSessions proves the browser session, not just
// API tokens, is metered: a script driving the console with a session cookie
// runs out of budget and gets a clear rate-limit answer.
func TestDashboardBudgetChargesCookieSessions(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	app.dashboardLimiter = newWindowLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if status, _, _ := api.request("GET", "/api/admin/client-keys", nil); status != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, status)
		}
	}
	status, payload, header := api.request("GET", "/api/admin/client-keys", nil)
	if status != http.StatusTooManyRequests || payload["error"].(map[string]any)["code"] != "rate_limited" {
		t.Fatalf("over budget = %d %v, want 429 rate_limited", status, payload)
	}
	if header.Get("Retry-After") == "" {
		t.Fatal("a rate-limited console response must say when to retry")
	}
}

// TestDashboardHeavyBudgetIsStricter proves the whole-table endpoints have their
// own, much smaller allowance, so a client cannot spend the account's entire
// budget exporting CSV.
func TestDashboardHeavyBudgetIsStricter(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	app.dashboardHeavyLimiter = newWindowLimiter(2, time.Minute)
	for i := 0; i < 2; i++ {
		if status, _, _ := api.request("GET", "/api/admin/activity/export", nil); status != http.StatusOK {
			t.Fatalf("export %d = %d, want 200", i, status)
		}
	}
	status, payload, _ := api.request("GET", "/api/admin/activity/export", nil)
	if status != http.StatusTooManyRequests || payload["error"].(map[string]any)["code"] != "rate_limited" {
		t.Fatalf("third export = %d %v, want 429 rate_limited", status, payload)
	}
	// Ordinary reads are unaffected by the heavy budget.
	if status, _, _ := api.request("GET", "/api/admin/client-keys", nil); status != http.StatusOK {
		t.Fatalf("ordinary read after heavy limit = %d, want 200", status)
	}
	// And the heavy classifier only claims whole-table routes: the ordinary
	// client-key list is not one, whatever its query string.
	if isDashboardHeavy(mustRequest(t, "/api/admin/client-keys?all=1")) {
		t.Fatal("the client-key list is not a whole-table report")
	}
	if !isDashboardHeavy(mustRequest(t, "/api/admin/models/abc/activity/export")) {
		t.Fatal("activity CSV exports are whole-table reports")
	}
	if !isDashboardHeavy(mustRequest(t, "/api/admin/usage")) {
		t.Fatal("the usage aggregate is a whole-table report")
	}
}

// TestDashboardLiveStreamIsNotChargedToBudget proves a dashboard tab sitting
// open on the live activity stream does not consume request budget: it is
// bounded as a concurrency limit instead. Without this, an idle tab would lock
// the user out of their own console.
func TestDashboardLiveStreamIsNotChargedToBudget(t *testing.T) {
	t.Parallel()
	app, api, accountID := hostedAccountServer(t)
	app.dashboardLimiter = newWindowLimiter(2, time.Minute)

	// Spend the account's whole request budget.
	for i := 0; i < 2; i++ {
		if status, _, _ := api.request("GET", "/api/admin/client-keys", nil); status != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, status)
		}
	}
	if status, _, _ := api.request("GET", "/api/admin/client-keys", nil); status != http.StatusTooManyRequests {
		t.Fatalf("expected the budget to be spent, got %d", status)
	}

	// The live stream lives on its own concurrency limiter, so it is unaffected.
	key := "live\x00" + accountID
	for i := 0; i < dashboardLiveConcurrency; i++ {
		q := app.dashboardLiveLimiter.takeLive(key)
		if !q.Allowed {
			t.Fatalf("live connection %d refused while the request budget was spent", i)
		}
	}
	if q := app.dashboardLiveLimiter.takeLive(key); q.Allowed {
		t.Fatal("the live concurrency cap must still apply")
	}
	for i := 0; i < dashboardLiveConcurrency; i++ {
		app.dashboardLiveLimiter.releaseLive(key)
	}
	if q := app.dashboardLiveLimiter.takeLive(key); !q.Allowed {
		t.Fatal("a closed stream must free its slot")
	}
}

// mustRequest builds a GET request for the given console path.
func mustRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("GET", path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
