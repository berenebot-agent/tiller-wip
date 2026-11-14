package server

import (
	"encoding/json"
	"github.com/tiller-router/tiller-router/internal/config"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// TestDashboardBudgetChargesEveryConsoleRouteOnce proves the budget is applied
// at the principal, not per registration: a cookie route, a session-identity
// route and a token-lifecycle route each spend exactly one unit. It fails if a
// route is missed (remaining does not drop) or charged twice (remaining drops
// by two, silently halving the account's allowance).
func TestDashboardBudgetChargesEveryConsoleRouteOnce(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	app.dashboardLimiter = newWindowLimiter(10, time.Minute)

	for i, path := range []string{
		"/api/admin/client-keys", // requireManage -> requireAdmin
		"/api/auth/account",      // requireUser
		"/api/admin/api-tokens",  // requireAdmin
		"/api/auth/account/plan", // requireUser
	} {
		status, payload, header := api.request("GET", path, nil)
		if status != http.StatusOK {
			t.Fatalf("%s = %d %v, want 200", path, status, payload)
		}
		if got, want := header.Get("X-RateLimit-Remaining"), strconv.Itoa(10-i-1); got != want {
			t.Fatalf("%s remaining = %q, want %q", path, got, want)
		}
	}
}

// TestDashboardBudgetCoversAccountExport proves the whole-account export is
// budgeted. It is named in the heavy list but was registered outside the wrapper
// that charges, so it could be called in a loop for free.
func TestDashboardBudgetCoversAccountExport(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	app.dashboardHeavyLimiter = newWindowLimiter(1, time.Minute)
	if status, payload, _ := api.request("GET", "/api/auth/account/export", nil); status != http.StatusOK {
		t.Fatalf("first export = %d %v, want 200", status, payload)
	}
	status, payload, _ := api.request("GET", "/api/auth/account/export", nil)
	if status != http.StatusTooManyRequests || payload["error"].(map[string]any)["code"] != "rate_limited" {
		t.Fatalf("second export = %d %v, want 429 rate_limited", status, payload)
	}
}

// TestDashboardBudgetDoesNotLockOutSessionProbe proves the account budget cannot
// refuse the request that tells the console who the viewer is. The budget is
// charged at the principal, so it also covers /api/*/session; if a spent budget
// answers that probe with 429 the SPA treats it as a signed-out state, paints
// the login card, and the user is locked out of their own console until the
// window rolls over -- which a normal dashboard tab (live stream plus polling)
// triggers on its own.
//
// It drives chargeDashboardBudget directly: that is the decision point both
// requireAdmin and requireUser share, and it keeps the test independent of which
// mode registers which probe route.
func TestDashboardBudgetDoesNotLockOutSessionProbe(t *testing.T) {
	t.Parallel()
	app, _, accountID := hostedAccountServer(t)
	app.dashboardLimiter = newWindowLimiter(1, time.Minute)

	// Spend the whole budget on an ordinary console route.
	ordinary := mustRequest(t, "/api/admin/client-keys")
	if !app.chargeDashboardBudget(httptest.NewRecorder(), ordinary, accountID) {
		t.Fatal("the first ordinary request should be admitted")
	}
	if app.chargeDashboardBudget(httptest.NewRecorder(), ordinary, accountID) {
		t.Fatal("expected the budget to be spent")
	}

	// Both identity probes still answer: a spent budget is a reason to slow
	// down, never to claim the session is gone.
	for _, path := range []string{"/api/admin/session", "/api/auth/session"} {
		if !app.chargeDashboardBudget(httptest.NewRecorder(), mustRequest(t, path), accountID) {
			t.Fatalf("%s was refused while the budget was spent; the session probe must not be budgeted", path)
		}
	}

	// The exemption is narrow: an ordinary console read still refuses, so the
	// budget itself is untouched.
	if app.chargeDashboardBudget(httptest.NewRecorder(), ordinary, accountID) {
		t.Fatal("an ordinary console read must still be refused while the budget is spent")
	}
	// And the refusal really writes the rate-limit answer.
	rec := httptest.NewRecorder()
	if app.chargeDashboardBudget(rec, ordinary, accountID) || rec.Code != http.StatusTooManyRequests {
		t.Fatalf("refusal = %d, want 429", rec.Code)
	}
}

// TestDashboardBudgetExemptionCoversOnlySessionPaths proves the exemption stays
// narrow: it names the two session probes and nothing else, so a route cannot
// drift out of the budget by accident.
func TestDashboardBudgetExemptionCoversOnlySessionPaths(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/api/admin/session", "/api/auth/session"} {
		if !isDashboardSessionProbe(mustRequest(t, path)) {
			t.Fatalf("%s should be exempt from the account budget", path)
		}
	}
	for _, path := range []string{
		"/api/admin/client-keys",
		"/api/admin/usage",
		"/api/auth/account",
		"/api/auth/account/sessions/revoke-all",
		"/api/admin/api-tokens",
	} {
		if isDashboardSessionProbe(mustRequest(t, path)) {
			t.Fatalf("%s must still spend the account budget", path)
		}
	}
	// Only the identity read is exempt: a write to the same path (logout, which
	// mutates) is ordinary console traffic and stays budgeted.
	write, err := http.NewRequest("DELETE", "/api/admin/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	if isDashboardSessionProbe(write) {
		t.Fatal("a state-changing session call must still spend the account budget")
	}
}

// TestClientsViewIsOneBudgetUnitAndMatchesTheStandaloneEndpoints proves the
// Clients screen's combined read costs a single unit and returns exactly what
// the five separate endpoints return.
func TestClientsViewIsOneBudgetUnitAndMatchesTheStandaloneEndpoints(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	app.dashboardLimiter = newWindowLimiter(20, time.Minute)

	status, view, header := api.request("GET", "/api/admin/clients-view", nil)
	if status != http.StatusOK {
		t.Fatalf("clients-view = %d %v, want 200", status, view)
	}
	if got := header.Get("X-RateLimit-Remaining"); got != "19" {
		t.Fatalf("clients-view remaining = %q, want 19 (one unit)", got)
	}
	for key, path := range map[string]string{
		"client_keys":    "/api/admin/client-keys?limit=200&search=&group=",
		"models":         "/api/admin/models?all=1",
		"virtual_models": "/api/admin/virtual-models?limit=200",
		"providers":      "/api/admin/providers?limit=200",
		"templates":      "/api/admin/catalogue-templates",
	} {
		_, want, _ := api.request("GET", path, nil)
		got, ok := view[key]
		if !ok {
			t.Fatalf("clients-view is missing %q", key)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("%s differs from %s:\n got %s\nwant %s", key, path, gotJSON, wantJSON)
		}
	}
}

func TestConsoleBudgetDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	if got := consoleBudget(config.Config{}); got != dashboardRequestsPerMinute {
		t.Fatalf("default = %d, want %d", got, dashboardRequestsPerMinute)
	}
	if got := consoleBudget(config.Config{ConsoleRequestsPerMinute: 500}); got != 500 {
		t.Fatalf("override = %d, want 500", got)
	}
}
