package server

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

// hostedPlatformAPI starts an authenticated platform (operator) API over the
// same handler as the hosted customer harness.
func hostedPlatformAPI(t *testing.T, app *Server) *testAPI {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	papi := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := papi.request("POST", "/api/platform/session", map[string]any{"username": "platform-admin", "password": "platform-secret"})
	if status != 200 {
		t.Fatalf("platform login: %d %v", status, payload)
	}
	papi.csrf = payload["csrf_token"].(string)
	return papi
}

// requestWithIdentity builds a request carrying an authenticated client
// identity in its context, as requireClient would attach.
func requestWithIdentity(accountID, clientID string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx := context.WithValue(req.Context(), accountKey, accountID)
	ctx = context.WithValue(ctx, clientKey, auth.ClientIdentity{ID: clientID, AccountID: accountID})
	return req.WithContext(ctx)
}

func TestPlatformPlansListUpdateAndValidation(t *testing.T) {
	app, _, accountID := hostedServerHarness(t, false)
	papi := hostedPlatformAPI(t, app)

	status, payload, _ := papi.request("GET", "/api/platform/plans", nil)
	if status != 200 {
		t.Fatalf("list plans: %d %v", status, payload)
	}
	if data, _ := payload["data"].([]any); len(data) == 0 {
		t.Fatal("plan catalogue is empty")
	}

	// A valid update replaces the caps.
	status, payload, _ = papi.request("PUT", "/api/platform/plans/free", map[string]any{
		"max_providers": 3, "max_client_keys": 5, "max_virtual_models": 5,
		"max_concurrent_streams": 5, "activity_retention_days": 7, "monthly_requests": 10000,
	})
	if status != 200 {
		t.Fatalf("update plan: %d %v", status, payload)
	}
	plan, err := app.storeHandle().EntitlementsForAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.MonthlyRequests != 10000 {
		t.Fatalf("monthly_requests = %d, want 10000", plan.MonthlyRequests)
	}

	// Values below -1 are rejected with invalid_limits.
	status, payload, _ = papi.request("PUT", "/api/platform/plans/free", map[string]any{
		"max_providers": -2, "max_client_keys": 5, "max_virtual_models": 5,
		"max_concurrent_streams": 5, "activity_retention_days": 7, "monthly_requests": 10000,
	})
	if status != 400 || payload["error"].(map[string]any)["code"] != "invalid_limits" {
		t.Fatalf("invalid limits: %d %v, want 400 invalid_limits", status, payload)
	}

	// An unknown plan name is a 404.
	status, payload, _ = papi.request("PUT", "/api/platform/plans/nope", map[string]any{
		"max_providers": 1, "max_client_keys": 1, "max_virtual_models": 1,
		"max_concurrent_streams": 1, "activity_retention_days": 1, "monthly_requests": 1,
	})
	if status != 404 || payload["error"].(map[string]any)["code"] != "plan_not_found" {
		t.Fatalf("unknown plan: %d %v, want 404 plan_not_found", status, payload)
	}

	// The update is audited.
	status, audit, _ := papi.request("GET", "/api/platform/audit", nil)
	if status != 200 {
		t.Fatalf("platform audit: %d %v", status, audit)
	}
	if !auditContains(audit, "platform.plan_changed") {
		t.Fatalf("no platform.plan_changed audit event: %v", audit)
	}
}

func TestAccountPlanEndpointShowsCapsAndUsage(t *testing.T) {
	app, api, accountID := hostedServerHarness(t, false)

	sc := app.storeHandle().For(accountID)
	if err := sc.CreateProvider(context.Background(), store.CreateProviderInput{ID: "p1", Name: "p1", Type: "generic-openai", BaseURL: "https://example.com/v1", Enabled: true, Protocols: "chat"}); err != nil {
		t.Fatal(err)
	}
	if err := sc.IncrementUsageCounter(context.Background(), store.UsagePeriod(time.Now())); err != nil {
		t.Fatal(err)
	}

	status, payload, _ := api.request("GET", "/api/auth/account/plan", nil)
	if status != 200 {
		t.Fatalf("account plan: %d %v", status, payload)
	}
	if payload["plan"] != "free" {
		t.Fatalf("plan = %v, want free", payload["plan"])
	}
	limits, _ := payload["limits"].(map[string]any)
	for _, key := range []string{"max_providers", "max_client_keys", "max_virtual_models", "max_concurrent_streams", "activity_retention_days", "monthly_requests"} {
		if _, ok := limits[key]; !ok {
			t.Fatalf("limits missing %q: %v", key, limits)
		}
	}
	usage, _ := payload["usage"].(map[string]any)
	if usage["providers"].(float64) != 1 {
		t.Fatalf("usage.providers = %v, want 1", usage["providers"])
	}
	if usage["monthly_requests"].(float64) != 1 {
		t.Fatalf("usage.monthly_requests = %v, want 1", usage["monthly_requests"])
	}
	if payload["period"] != store.UsagePeriod(time.Now()) {
		t.Fatalf("period = %v, want %s", payload["period"], store.UsagePeriod(time.Now()))
	}
}

func TestAccountPlanAssignAndErrors(t *testing.T) {
	app, _, accountID := hostedServerHarness(t, false)
	papi := hostedPlatformAPI(t, app)
	ctx := context.Background()
	if _, err := app.db.SQL.Exec(`INSERT INTO plans(name,max_providers,max_client_keys,max_virtual_models,max_concurrent_streams,activity_retention_days,monthly_requests,updated_at) VALUES('pro',-1,-1,-1,-1,90,-1,?)`, database.Now()); err != nil {
		t.Fatal(err)
	}

	status, payload, _ := papi.request("POST", "/api/platform/accounts/"+accountID+"/plan", map[string]any{"plan": "pro"})
	if status != 200 {
		t.Fatalf("assign plan: %d %v", status, payload)
	}
	plan, err := app.storeHandle().EntitlementsForAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Name != "pro" {
		t.Fatalf("account plan = %q, want pro", plan.Name)
	}

	// Unknown plan → 404 plan_not_found.
	status, payload, _ = papi.request("POST", "/api/platform/accounts/"+accountID+"/plan", map[string]any{"plan": "nope"})
	if status != 404 || payload["error"].(map[string]any)["code"] != "plan_not_found" {
		t.Fatalf("unknown plan assign: %d %v, want 404 plan_not_found", status, payload)
	}
	// Unknown account → 404 account_not_found.
	status, payload, _ = papi.request("POST", "/api/platform/accounts/does-not-exist/plan", map[string]any{"plan": "pro"})
	if status != 404 || payload["error"].(map[string]any)["code"] != "account_not_found" {
		t.Fatalf("unknown account assign: %d %v, want 404 account_not_found", status, payload)
	}
	// Audit recorded.
	_, audit, _ := papi.request("GET", "/api/platform/audit", nil)
	if !auditContains(audit, "platform.account_plan_changed") {
		t.Fatalf("no platform.account_plan_changed audit event: %v", audit)
	}
}

func TestMonthlyLimitEnforcementReturns429(t *testing.T) {
	app, _, accountID := hostedServerHarness(t, false)
	ctx := context.Background()
	if err := app.storeHandle().UpdatePlan(ctx, store.Plan{Name: "free", MaxProviders: -1, MaxClientKeys: -1, MaxVirtualModels: -1, MaxConcurrentStreams: -1, ActivityRetentionDays: 7, MonthlyRequests: 1}); err != nil {
		t.Fatal(err)
	}
	sc := app.storeHandle().For(accountID)
	if err := sc.IncrementUsageCounter(ctx, store.UsagePeriod(time.Now())); err != nil {
		t.Fatal(err)
	}

	req := requestWithIdentity(accountID, "ck-1")
	rec := httptest.NewRecorder()
	identity := auth.ClientIdentity{ID: "ck-1", AccountID: accountID}
	if !app.enforceHostedQuotas(rec, req, identity, false) {
		t.Fatal("monthly cap at limit did not reject")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("monthly rejection is missing Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "monthly_limit_exceeded") {
		t.Fatalf("body = %s, want monthly_limit_exceeded", rec.Body.String())
	}

	// Under the cap the request is allowed.
	if err := app.storeHandle().UpdatePlan(ctx, store.Plan{Name: "free", MaxProviders: -1, MaxClientKeys: -1, MaxVirtualModels: -1, MaxConcurrentStreams: -1, ActivityRetentionDays: 7, MonthlyRequests: 5}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	if app.enforceHostedQuotas(rec, req, identity, false) {
		t.Fatal("request under monthly cap was rejected")
	}
}

func TestConcurrencyLimitEnforcementReturns429(t *testing.T) {
	app, _, accountID := hostedServerHarness(t, false)
	ctx := context.Background()
	if err := app.storeHandle().UpdatePlan(ctx, store.Plan{Name: "free", MaxProviders: -1, MaxClientKeys: -1, MaxVirtualModels: -1, MaxConcurrentStreams: 1, ActivityRetentionDays: 7, MonthlyRequests: -1}); err != nil {
		t.Fatal(err)
	}
	// One live ticket for the account at the cap.
	app.inflight.clientStart(accountID, "ck-1", "route-1", "model-a")

	req := requestWithIdentity(accountID, "ck-1")
	rec := httptest.NewRecorder()
	identity := auth.ClientIdentity{ID: "ck-1", AccountID: accountID}
	if !app.enforceHostedQuotas(rec, req, identity, false) {
		t.Fatal("concurrency cap at limit did not reject")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("concurrency rejection is missing Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "stream_limit_exceeded") {
		t.Fatalf("body = %s, want stream_limit_exceeded", rec.Body.String())
	}
	app.inflight.clientEnd(accountID, "ck-1", "route-1", false)

	// Under the cap (ticket released) the request is allowed.
	rec = httptest.NewRecorder()
	if app.enforceHostedQuotas(rec, req, identity, false) {
		t.Fatal("request under concurrency cap was rejected")
	}
}

func TestConcurrencyLimitIsAccountScoped(t *testing.T) {
	app, _, accountA := hostedServerHarness(t, false)
	// A second account with its own client key.
	const accountB = "acct-plan-b-server"
	if _, err := app.db.SQL.Exec(`INSERT INTO accounts(id,plan,status,created_at,updated_at) VALUES(?,'free','active',?,?)`, accountB, database.Now(), database.Now()); err != nil {
		t.Fatal(err)
	}
	if err := app.storeHandle().UpdatePlan(context.Background(), store.Plan{Name: "free", MaxProviders: -1, MaxClientKeys: -1, MaxVirtualModels: -1, MaxConcurrentStreams: 1, ActivityRetentionDays: 7, MonthlyRequests: -1}); err != nil {
		t.Fatal(err)
	}
	// Account A is at its cap; account B has no tickets.
	app.inflight.clientStart(accountA, "ck-a", "route-1", "model-a")
	reqB := requestWithIdentity(accountB, "ck-b")
	rec := httptest.NewRecorder()
	if app.enforceHostedQuotas(rec, reqB, auth.ClientIdentity{ID: "ck-b", AccountID: accountB}, false) {
		t.Fatal("account A's live ticket rejected account B's request")
	}
	app.inflight.clientEnd(accountA, "ck-a", "route-1", false)
}

func TestLocalModeSkipsQuotaEnforcement(t *testing.T) {
	app, _ := newLogWriterServer(t)
	if app.config.Mode == config.ModeHosted {
		t.Fatal("test server unexpectedly hosted")
	}
	// A hard zero cap in local mode must never reject.
	if err := app.storeHandle().UpdatePlan(context.Background(), store.Plan{Name: "free", MaxProviders: 0, MaxClientKeys: 0, MaxVirtualModels: 0, MaxConcurrentStreams: 0, ActivityRetentionDays: 7, MonthlyRequests: 0}); err != nil {
		t.Fatal(err)
	}
	accountID := database.LocalAccountID
	req := requestWithIdentity(accountID, "ck-1")
	rec := httptest.NewRecorder()
	identity := auth.ClientIdentity{ID: "ck-1", AccountID: accountID}
	if app.enforceHostedQuotas(rec, req, identity, false) {
		t.Fatal("local mode enforced a hosted quota")
	}
}

func TestLogWriterIncrementsUsageCounterPerRow(t *testing.T) {
	app, db := newLogWriterServer(t)
	w := newLogWriter(app.scopeFor, app.logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.start(ctx)
	for i := 0; i < 3; i++ {
		w.enqueue(writerRow("usage-row-" + string(rune('a'+i))))
	}
	period := store.UsagePeriod(time.Now())
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := db.SQL.QueryRow(`SELECT requests FROM usage_counters WHERE account_id=? AND period=?`, database.LocalAccountID, period).Scan(&n); err == nil && n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage counter did not reach 3 within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	w.wait()
}

func TestCreateLimitReturns409LimitExceeded(t *testing.T) {
	app, api, _ := hostedServerHarness(t, false)
	ctx := context.Background()
	// One client key allowed; unlimited everywhere else.
	if err := app.storeHandle().UpdatePlan(ctx, store.Plan{Name: "free", MaxProviders: -1, MaxClientKeys: 1, MaxVirtualModels: -1, MaxConcurrentStreams: -1, ActivityRetentionDays: 7, MonthlyRequests: -1}); err != nil {
		t.Fatal(err)
	}
	status, payload, _ := api.request("POST", "/api/admin/client-keys", map[string]any{"name": "first", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("first client key: %d %v", status, payload)
	}
	// The second create must be a 409 limit_exceeded, not a 500.
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "second", "type": "catalogue"})
	if status != http.StatusConflict {
		t.Fatalf("second client key: %d %v, want 409", status, payload)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "limit_exceeded" {
		t.Fatalf("second client key code = %v, want limit_exceeded (%v)", errObj["code"], payload)
	}
	if errObj["kind"] != "client_keys" {
		t.Fatalf("limit kind = %v, want client_keys", errObj["kind"])
	}
}

func auditContains(payload map[string]any, event string) bool {
	data, _ := payload["data"].([]any)
	for _, raw := range data {
		if row, ok := raw.(map[string]any); ok && row["event"] == event {
			return true
		}
	}
	return false
}
