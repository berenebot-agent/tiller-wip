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

func TestRefreshIfDueUnknownProvider(t *testing.T) {
	p := NewPoller(http.DefaultClient, func(context.Context, ProviderRef) (string, error) { return "", nil })
	if _, ok := p.RefreshIfDue(context.Background(), "acct", "missing", false, time.Now()); ok {
		t.Fatal("unknown provider should not poll")
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

func TestFetchOllamaCloudNestedFractions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usage" {
			t.Errorf("path = %q, want /api/usage", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		// Verified shape: usage is a fraction (0-1).
		w.Write([]byte(`{"limits":{"session":{"usage":0.42},"weekly":{"usage":0.55}}}`))
	}))
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

func TestFetchOllamaCloudTopLevelFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"session":0.1,"weekly":0.2}`))
	}))
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"limits":[{"label":"session","used_percent":30}]}`))
	}))
	defer srv.Close()
	snap, err := fetchOllamaCloud(context.Background(), srv.Client(), Credential{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Label != "session" || *snap.Windows[0].UsedPercent != 30 {
		t.Fatalf("windows = %+v", snap.Windows)
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
