package providerquota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupports(t *testing.T) {
	for _, ty := range []string{"codex-subscription", "claude-subscription", "github-copilot", "zai", "ollama-cloud", "commandcode"} {
		if !Supports(ty) {
			t.Errorf("Supports(%q) = false, want true", ty)
		}
	}
	if Supports("openai") {
		t.Error("Supports(openai) = true, want false")
	}
}

func TestRefreshIfDueCadence(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"five_hour":{"utilization":42},"seven_day":{"utilization":10}}`))
	}))
	defer srv.Close()

	// The Claude adapter hardcodes the production URL, so exercise the cadence
	// through a test adapter instead.
	adapters["test-quota"] = func(_ context.Context, client *http.Client, _ Credential) (Snapshot, error) {
		atomic.AddInt64(&calls, 1)
		pct := 50.0
		return Snapshot{Available: true, Windows: []Window{{Label: "5h", UsedPercent: &pct}}}, nil
	}
	defer delete(adapters, "test-quota")

	p := NewPoller(http.DefaultClient, func(context.Context, ProviderRef) (string, error) { return "tok", nil })
	p.Register(ProviderRef{AccountID: "acct", ProviderID: "prov", Type: "test-quota"})

	now := time.Now().UTC()
	// First call is always due (no prior poll).
	if _, polled := p.RefreshIfDue(context.Background(), "acct", "prov", false, now); !polled {
		t.Fatal("first refresh should poll")
	}
	// Immediately after, background cadence is not due.
	if _, polled := p.RefreshIfDue(context.Background(), "acct", "prov", false, now.Add(time.Second)); polled {
		t.Fatal("second refresh within background interval should not poll")
	}
	// force respects MinInterval, so a forced refresh right away is suppressed.
	if _, polled := p.RefreshIfDue(context.Background(), "acct", "prov", true, now.Add(2*time.Second)); polled {
		t.Fatal("forced refresh within MinInterval should not poll")
	}
	// force after MinInterval polls.
	if _, polled := p.RefreshIfDue(context.Background(), "acct", "prov", true, now.Add(MinInterval+time.Second)); !polled {
		t.Fatal("forced refresh after MinInterval should poll")
	}
	// Marking active tightens the interval to ActiveInterval.
	p.MarkActive("acct", "prov", now.Add(MinInterval+time.Second))
	if _, polled := p.RefreshIfDue(context.Background(), "acct", "prov", false, now.Add(MinInterval+ActiveInterval+time.Second)); !polled {
		t.Fatal("active provider should poll at the active interval")
	}
	if got := atomic.LoadInt64(&calls); got != 3 {
		t.Fatalf("adapter calls = %d, want 3", got)
	}
}

// TestFailureRetryCadence proves a provider whose last poll failed is retried
// on the short FailureRetry cadence rather than waiting a full background
// interval, so a transient blip does not leave the card unavailable for 30 min.
func TestFailureRetryCadence(t *testing.T) {
	var calls int64
	adapters["test-flaky"] = func(_ context.Context, _ *http.Client, _ Credential) (Snapshot, error) {
		n := atomic.AddInt64(&calls, 1)
		if n == 1 {
			// First poll fails: no windows → unavailable.
			return Snapshot{}, nil
		}
		pct := 42.0
		return Snapshot{Available: true, Windows: []Window{{Label: "5h", UsedPercent: &pct}}}, nil
	}
	defer delete(adapters, "test-flaky")

	p := NewPoller(http.DefaultClient, func(context.Context, ProviderRef) (string, error) { return "tok", nil })
	p.Register(ProviderRef{AccountID: "acct", ProviderID: "prov", Type: "test-flaky"})

	now := time.Now().UTC()
	// First poll runs (and fails, as an empty snapshot).
	snap, polled := p.RefreshIfDue(context.Background(), "acct", "prov", false, now)
	if !polled || snap.Available {
		t.Fatalf("first poll = %+v, polled=%v; want an unavailable result", snap, polled)
	}
	// A background refresh before FailureRetry is not due (would be a 30-min
	// wait were the failure not tracked).
	if _, polled := p.RefreshIfDue(context.Background(), "acct", "prov", false, now.Add(time.Second)); polled {
		t.Fatal("failed provider should not poll again within FailureRetry")
	}
	// After FailureRetry it is retried, and now succeeds.
	snap, polled = p.RefreshIfDue(context.Background(), "acct", "prov", false, now.Add(FailureRetry+time.Second))
	if !polled || !snap.Available {
		t.Fatalf("retry = %+v, polled=%v; want an available result", snap, polled)
	}
	// A healthy provider returns to the normal background cadence.
	if _, polled := p.RefreshIfDue(context.Background(), "acct", "prov", false, now.Add(FailureRetry+2*time.Second)); polled {
		t.Fatal("recovered provider should not poll on the failure cadence")
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("adapter calls = %d, want 2", got)
	}
}

func TestRefreshIfDueUnknownProvider(t *testing.T) {
	p := NewPoller(http.DefaultClient, func(context.Context, ProviderRef) (string, error) { return "", nil })
	if _, ok := p.RefreshIfDue(context.Background(), "acct", "missing", false, time.Now()); ok {
		t.Fatal("unknown provider should not poll")
	}
}

// TestReconcileUnregistersAbsentProviders is the regression for quota cards
// persisting after a provider was deleted or disabled: registration was
// add-only and Unregister had no callers.
func TestReconcileUnregistersAbsentProviders(t *testing.T) {
	adapters["test-reconcile"] = func(context.Context, *http.Client, Credential) (Snapshot, error) {
		pct := 10.0
		return Snapshot{Available: true, Windows: []Window{{Label: "5h", UsedPercent: &pct}}}, nil
	}
	defer delete(adapters, "test-reconcile")

	p := NewPoller(http.DefaultClient, func(context.Context, ProviderRef) (string, error) { return "tok", nil })
	keep := ProviderRef{AccountID: "acct", ProviderID: "keep", Type: "test-reconcile"}
	drop := ProviderRef{AccountID: "acct", ProviderID: "drop", Type: "test-reconcile"}
	p.Reconcile([]ProviderRef{keep, drop})
	if _, polled := p.RefreshIfDue(context.Background(), "acct", "drop", false, time.Now()); !polled {
		t.Fatal("drop provider should poll")
	}
	if _, ok := p.Snapshot("acct", "drop"); !ok {
		t.Fatal("expected a cached snapshot before reconcile")
	}

	// drop is no longer configured; keep remains.
	p.Reconcile([]ProviderRef{keep})
	if _, ok := p.Snapshot("acct", "drop"); ok {
		t.Fatal("snapshot for a removed provider survived reconcile")
	}
	if _, ok := p.RefreshIfDue(context.Background(), "acct", "drop", false, time.Now()); ok {
		t.Fatal("removed provider should no longer poll")
	}
	if _, ok := p.RefreshIfDue(context.Background(), "acct", "keep", false, time.Now()); !ok {
		t.Fatal("remaining provider should still poll")
	}
}

// TestReconcileDoesNotInvalidateUnchangedRegistration proves the generation
// only moves when a registration actually changes, so a steady-state reconcile
// on every tick cannot discard in-flight poll results.
func TestReconcileDoesNotInvalidateUnchangedRegistration(t *testing.T) {
	p := NewPoller(http.DefaultClient, func(context.Context, ProviderRef) (string, error) { return "tok", nil })
	ref := ProviderRef{AccountID: "acct", ProviderID: "prov", Type: "codex-subscription"}
	p.Register(ref)
	p.mu.Lock()
	before := p.gen[key("acct", "prov")]
	p.mu.Unlock()
	for i := 0; i < 3; i++ {
		p.Reconcile([]ProviderRef{ref})
	}
	p.mu.Lock()
	after := p.gen[key("acct", "prov")]
	p.mu.Unlock()
	if before != after {
		t.Fatalf("unchanged reconcile moved generation %d -> %d", before, after)
	}
}

// TestUnregisterDiscardsInFlightPoll proves a poll that finishes after the
// provider was removed cannot republish its snapshot.
func TestUnregisterDiscardsInFlightPoll(t *testing.T) {
	release := make(chan struct{})
	adapters["test-late"] = func(ctx context.Context, _ *http.Client, _ Credential) (Snapshot, error) {
		<-release
		pct := 99.0
		return Snapshot{Available: true, Windows: []Window{{Label: "5h", UsedPercent: &pct}}}, nil
	}
	defer delete(adapters, "test-late")

	p := NewPoller(http.DefaultClient, func(context.Context, ProviderRef) (string, error) { return "tok", nil })
	p.Register(ProviderRef{AccountID: "acct", ProviderID: "prov", Type: "test-late"})

	done := make(chan struct{})
	go func() {
		p.RefreshIfDue(context.Background(), "acct", "prov", true, time.Now())
		close(done)
	}()
	// Wait until the adapter is definitely running, then remove the provider.
	for {
		p.mu.Lock()
		running := p.inflight[key("acct", "prov")]
		p.mu.Unlock()
		if running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	p.Unregister("acct", "prov")
	close(release)
	<-done

	if _, ok := p.Snapshot("acct", "prov"); ok {
		t.Fatal("a poll that completed after unregister republished a snapshot")
	}
}

func TestPollHydrateFailureIsUnavailable(t *testing.T) {
	adapters["test-quota2"] = func(context.Context, *http.Client, Credential) (Snapshot, error) {
		t.Fatal("adapter should not run when hydration fails")
		return Snapshot{}, nil
	}
	defer delete(adapters, "test-quota2")
	p := NewPoller(http.DefaultClient, func(context.Context, ProviderRef) (string, error) {
		return "", context.DeadlineExceeded
	})
	p.Register(ProviderRef{AccountID: "acct", ProviderID: "prov", Type: "test-quota2"})
	snap, _ := p.RefreshIfDue(context.Background(), "acct", "prov", false, time.Now())
	if snap.Available || snap.Reason == "" {
		t.Fatalf("expected unavailable snapshot, got %+v", snap)
	}
}

func TestParseClaudeWindow(t *testing.T) {
	util := 85.0
	w := claudeWindowView("5h", &claudeWindow{Utilization: &util, ResetsAt: "2026-09-30T12:00:00Z"})
	if w == nil || w.UsedPercent == nil || *w.UsedPercent != 85 {
		t.Fatalf("window = %+v", w)
	}
	if w.ResetsAt == nil || w.ResetsAt.UTC().Hour() != 12 {
		t.Fatalf("resets_at = %v", w.ResetsAt)
	}
}

// ollamaUsageOnlyServer answers /api/usage with body and 404s /api/balance, so
// the adapter's balance-first probe falls through to the legacy/usage path.
func ollamaUsageOnlyServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usage" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
}

func TestFetchOllamaCloudNestedFractions(t *testing.T) {
	srv := ollamaUsageOnlyServer(t, `{"limits":{"session":{"usage":0.42},"weekly":{"usage":0.55}}}`)
	defer srv.Close()
	snap, err := fetchOllamaCloud(context.Background(), srv.Client(), Credential{BaseURL: srv.URL, Credential: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v, want 2", snap.Windows)
	}
	if snap.Windows[0].Label != "session" || snap.Windows[0].UsedPercent == nil || *snap.Windows[0].UsedPercent != 42 {
		t.Fatalf("session window = %+v, want 42%%", snap.Windows[0])
	}
	if snap.Windows[1].Label != "weekly" || snap.Windows[1].UsedPercent == nil {
		t.Fatalf("weekly window = %+v, want 55%%", snap.Windows[1])
	}
	if got := *snap.Windows[1].UsedPercent; got != 55 {
		t.Fatalf("weekly pct = %v, want 55", got)
	}
}

// TestFetchOllamaCloudBalance is the regression for the /api/balance generation:
// remaining_percent (0-100) is inverted to used_percent, and resets_at is
// carried through.
func TestFetchOllamaCloudBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/balance" {
			t.Errorf("path = %q, want /api/balance", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"included":{"session":{"remaining_percent":72.34,"resets_at":"2026-10-10T11:00:00Z"},"weekly":{"remaining_percent":18.55,"resets_at":"2026-10-12T00:00:00Z"}},"purchased":{"balance_usd":0}}`))
	}))
	defer srv.Close()
	snap, err := fetchOllamaCloud(context.Background(), srv.Client(), Credential{BaseURL: srv.URL, Credential: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v, want 2 (balance preferred, no credits when 0)", snap.Windows)
	}
	if snap.Windows[0].Label != "session" || snap.Windows[0].UsedPercent == nil {
		t.Fatalf("session window = %+v", snap.Windows[0])
	}
	if got := *snap.Windows[0].UsedPercent; got < 27.65 || got > 27.67 {
		t.Fatalf("session used pct = %v, want ~27.66", got)
	}
	if snap.Windows[0].ResetsAt == nil || snap.Windows[0].ResetsAt.UTC().Hour() != 11 {
		t.Fatalf("session resets_at = %v, want 11:00Z", snap.Windows[0].ResetsAt)
	}
	if snap.Windows[1].Label != "weekly" || snap.Windows[1].UsedPercent == nil {
		t.Fatalf("weekly window = %+v", snap.Windows[1])
	}
	if got := *snap.Windows[1].UsedPercent; got < 81.4 || got > 81.5 {
		t.Fatalf("weekly used pct = %v, want ~81.45", got)
	}
}

// TestFetchOllamaCloudBalancePurchasedCovered: a non-zero purchased balance is
// surfaced as a "credits" window (remaining USD), not a percentage.
func TestFetchOllamaCloudBalancePurchasedCovered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/balance" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"included":{"session":{"remaining_percent":90}},"purchased":{"balance_usd":12.5}}`))
	}))
	defer srv.Close()
	snap, err := fetchOllamaCloud(context.Background(), srv.Client(), Credential{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v, want session + credits", snap.Windows)
	}
	credits := snap.Windows[1]
	if credits.Label != "credits" || credits.Remaining == nil || *credits.Remaining != 12.5 {
		t.Fatalf("credits window = %+v, want remaining 12.5", credits)
	}
	if credits.UsedPercent != nil {
		t.Fatalf("credits window should not carry a percentage, got %+v", credits)
	}
}

func TestFetchOllamaCloudTopLevelFallback(t *testing.T) {
	srv := ollamaUsageOnlyServer(t, `{"session":0.1,"weekly":0.2}`)
	defer srv.Close()
	snap, err := fetchOllamaCloud(context.Background(), srv.Client(), Credential{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 2 || snap.Windows[0].UsedPercent == nil || *snap.Windows[0].UsedPercent != 10 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
}

func TestFetchOllamaCloudLegacyArrayFallback(t *testing.T) {
	srv := ollamaUsageOnlyServer(t, `{"limits":[{"label":"session","used_percent":30}]}`)
	defer srv.Close()
	snap, err := fetchOllamaCloud(context.Background(), srv.Client(), Credential{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Label != "session" || *snap.Windows[0].UsedPercent != 30 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
}

// TestFetchOllamaCloudUsageShapeChangeNoLongerErrors: the modern /api/usage
// returns a request-count time series with no windows. Since /api/balance is
// probed first and succeeds with windows, that remains the source. When both
// endpoints respond but yield nothing, the result is an empty (not error)
// snapshot so the poller reports "format unavailable" rather than a hard error.
func TestFetchOllamaCloudEmptyShapeIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/balance" {
			w.Write([]byte(`{"included":{},"purchased":{"balance_usd":0}}`))
			return
		}
		w.Write([]byte(`{"range":"7d","scope":"self","totals":{"request_count":22395}}`))
	}))
	defer srv.Close()
	snap, err := fetchOllamaCloud(context.Background(), srv.Client(), Credential{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("empty shapes should not be an error: %v", err)
	}
	if len(snap.Windows) != 0 {
		t.Fatalf("windows = %+v, want none", snap.Windows)
	}
}

// requireCommandCodeHeaders asserts the headers Cloudflare and the API need:
// a non-default User-Agent and a bearer token.
func requireCommandCodeHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", got)
	}
	if got := r.Header.Get("User-Agent"); got != commandCodeUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, commandCodeUserAgent)
	}
}

func TestFetchCommandCode(t *testing.T) {
	resetAt := time.Now().Add(90 * time.Minute).UnixMilli()
	periodEnd := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("/alpha/billing/credits", func(w http.ResponseWriter, r *http.Request) {
		requireCommandCodeHeaders(t, r)
		w.Header().Set("Content-Type", "application/json")
		// Verified wire shape: windowLimits is top-level; 5h resetAt is 0 (no
		// active reset); credit balances use *Credits field names.
		w.Write([]byte(`{"credits":{"belowThreshold":false,"creditThreshold":0,` +
			`"monthlyCredits":0.11,"purchasedCredits":5,"freeCredits":1},` +
			`"windowLimits":{"limited":true,"exceeded":null,` +
			`"fiveHour":{"used":0,"cap":14,"exceeded":false,"resetAt":0},` +
			`"weekly":{"used":26.03,"cap":35,"exceeded":false,"resetAt":` + strconv.FormatInt(resetAt, 10) + `}},` +
			`"sandboxAccess":false,"sandboxMinutes":null}`))
	})
	mux.HandleFunc("/alpha/billing/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		requireCommandCodeHeaders(t, r)
		// currentPeriodEnd is the monthly reset the adapter surfaces.
		w.Write([]byte(`{"success":true,"data":{"planId":"individual-goat","currentPeriodEnd":"` + periodEnd.Format(time.RFC3339) + `"}}`))
	})
	mux.HandleFunc("/alpha/usage/summary", func(w http.ResponseWriter, r *http.Request) {
		requireCommandCodeHeaders(t, r)
		// Period spend; combined with the 0.11 remaining this anchors a
		// ~70-credit monthly pool (GOAT) at ~99.8% used.
		w.Write([]byte(`{"totalMonthlyCredits":69.89,"totalCost":69.89,"periodBasis":"billing-period"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Base URL mimics the provider API path; the adapter must use the origin.
	snap, err := fetchCommandCode(context.Background(), srv.Client(), Credential{BaseURL: srv.URL + "/provider/v1", Credential: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Plan != "individual-goat" {
		t.Fatalf("plan = %q, want individual-goat", snap.Plan)
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("windows = %+v, want 3", snap.Windows)
	}
	fiveHour := snap.Windows[0]
	if fiveHour.Label != "5h" || fiveHour.UsedPercent == nil || *fiveHour.UsedPercent != 0 {
		t.Fatalf("5h window = %+v, want 0%%", fiveHour)
	}
	if fiveHour.Limit == nil || *fiveHour.Limit != 14 || fiveHour.Remaining == nil || *fiveHour.Remaining != 14 {
		t.Fatalf("5h window limit/remaining = %+v", fiveHour)
	}
	if fiveHour.ResetsAt != nil {
		t.Fatalf("5h resets_at = %v, want nil for resetAt 0", fiveHour.ResetsAt)
	}
	weekly := snap.Windows[1]
	if weekly.Label != "weekly" || weekly.UsedPercent == nil {
		t.Fatalf("weekly window = %+v", weekly)
	}
	if weekly.ResetsAt == nil || !weekly.ResetsAt.Equal(time.UnixMilli(resetAt).UTC()) {
		t.Fatalf("weekly resets_at = %v, want %v", weekly.ResetsAt, time.UnixMilli(resetAt).UTC())
	}
	monthly := snap.Windows[2]
	if monthly.Label != "monthly" {
		t.Fatalf("monthly window = %+v, want label monthly", monthly)
	}
	if monthly.Limit == nil || *monthly.Limit != 70 || monthly.Remaining == nil || *monthly.Remaining != 0.11 {
		t.Fatalf("monthly limit/remaining = %+v, want cap 70 remaining 0.11", monthly)
	}
	if monthly.UsedPercent == nil || *monthly.UsedPercent < 99 || *monthly.UsedPercent > 100 {
		t.Fatalf("monthly used_percent = %v, want ~99.8", monthly.UsedPercent)
	}
	if monthly.ResetsAt == nil || !monthly.ResetsAt.Equal(periodEnd) {
		t.Fatalf("monthly resets_at = %v, want %v", monthly.ResetsAt, periodEnd)
	}
}

func TestFetchCommandCodePlanFailureStillReportsWindows(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/alpha/billing/credits", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"credits":{"monthlyCredits":10},` +
			`"windowLimits":{"limited":true,"fiveHour":{"used":1,"cap":10}}}`))
	})
	mux.HandleFunc("/alpha/billing/subscriptions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	snap, err := fetchCommandCode(context.Background(), srv.Client(), Credential{BaseURL: srv.URL, Credential: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Plan != "" {
		t.Fatalf("plan = %q, want empty on failure", snap.Plan)
	}
	// The usage summary is unavailable here, so only the rolling window shows;
	// a plan-endpoint failure never discards it.
	if len(snap.Windows) != 1 || snap.Windows[0].Label != "5h" {
		t.Fatalf("windows = %+v, want only the 5h window", snap.Windows)
	}
}

// TestFetchCommandCodePayGoNoSubscriptionDetail verifies pay-as-you-go (no
// subscription detail in the response) surfaces windows but no monthly window.
func TestFetchCommandCodePayGoNoSubscriptionDetail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/alpha/billing/credits", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"credits":{"belowThreshold":false,"creditThreshold":0},` +
			`"windowLimits":{"limited":false,"fiveHour":{"used":0,"cap":14}}}`))
	})
	mux.HandleFunc("/alpha/billing/subscriptions", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"success":true,"data":null}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	snap, err := fetchCommandCode(context.Background(), srv.Client(), Credential{BaseURL: srv.URL, Credential: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Plan != "" {
		t.Fatalf("plan = %q, want empty", snap.Plan)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Label != "5h" {
		t.Fatalf("windows = %+v, want only the 5h window", snap.Windows)
	}
}

// TestFetchCommandCodeMonthlyNoPeriodEnd verifies a subscriptions response
// without currentPeriodEnd yields a monthly window with no reset time.
func TestFetchCommandCodeMonthlyNoPeriodEnd(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/alpha/billing/credits", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"credits":{"monthlyCredits":5},` +
			`"windowLimits":{"limited":false,"fiveHour":{"used":0,"cap":14}}}`))
	})
	mux.HandleFunc("/alpha/billing/subscriptions", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"success":true,"data":{"planId":"individual-goat"}}`))
	})
	mux.HandleFunc("/alpha/usage/summary", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"totalMonthlyCredits":10}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	snap, err := fetchCommandCode(context.Background(), srv.Client(), Credential{BaseURL: srv.URL, Credential: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	var monthly *Window
	for i := range snap.Windows {
		if snap.Windows[i].Label == "monthly" {
			monthly = &snap.Windows[i]
		}
	}
	if monthly == nil {
		t.Fatalf("windows = %+v, want a monthly window", snap.Windows)
	}
	if monthly.ResetsAt != nil {
		t.Fatalf("monthly resets_at = %v, want nil when currentPeriodEnd is absent", monthly.ResetsAt)
	}
}

func TestFetchZAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// unit 3 = minutes-per-unit multiplier 60, number 5 => 300 min = 5h.
		w.Write([]byte(`{"success":true,"code":200,"data":{"planName":"Pro","limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":37.5}]}}`))
	}))
	defer srv.Close()
	snap, err := fetchZAI(context.Background(), srv.Client(), Credential{BaseURL: srv.URL, Credential: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Plan != "Pro" || len(snap.Windows) != 1 || snap.Windows[0].UsedPercent == nil || *snap.Windows[0].UsedPercent != 37.5 {
		t.Fatalf("snap = %+v", snap)
	}
	if snap.Windows[0].Label != "5h" {
		t.Fatalf("window label = %q, want 5h", snap.Windows[0].Label)
	}
}
