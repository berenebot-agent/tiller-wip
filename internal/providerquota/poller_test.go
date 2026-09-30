package providerquota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupports(t *testing.T) {
	for _, ty := range []string{"codex-subscription", "claude-subscription", "github-copilot", "zai", "ollama-cloud"} {
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
