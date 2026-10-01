package providerquota

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// commandCodeUserAgent is required to reach the Command Code billing API. The
// host sits behind Cloudflare, which rejects the Go default client signature
// (default User-Agent) with HTTP 403 "error code: 1010". The official CLI sends
// "cli"; any non-default browser-like value is accepted.
const commandCodeUserAgent = "cli"

// fetchCommandCode reads the Command Code plan's rolling usage windows. The
// endpoint is undocumented in the public Provider API docs; it is the internal
// billing API the official CLI uses to render `/usage`. The verified shape
// (command-code CLI, live account) is:
//
//	GET {origin}/alpha/billing/credits
//	{"credits":{"monthlyCredits":0.11,"purchasedCredits":0,"freeCredits":0},
//	 "windowLimits":{"limited":true,"exceeded":null,
//	   "fiveHour":{"used":0,"cap":14,"exceeded":false,"resetAt":0},
//	   "weekly":{"used":26.03,"cap":35,"exceeded":false,"resetAt":1790862172858}}}
//
// The monthly subscription pool is not reported as a cap; it is derived from
// the billing-period spend plus the remaining monthly balance reported by:
//
//	GET {origin}/alpha/usage/summary
//	{"totalMonthlyCredits":69.92,"totalCost":69.92,...}
//
// so a nearly-spent month (70 - 0.11 of a 70-credit pool) renders as a nearly
// full bar. This mirrors the CLI, which computes the same used/cap pair.
//
// windowLimits is top-level (not nested under credits). used/cap are
// credit-value units; resetAt is epoch milliseconds, and 0 means the window has
// no active reset. The plan id comes from /alpha/billing/subscriptions and is
// best-effort: a failure there must not discard the windows.
func fetchCommandCode(ctx context.Context, client *http.Client, cred Credential) (Snapshot, error) {
	base := strings.TrimRight(cred.BaseURL, "/")
	if base == "" {
		base = "https://api.commandcode.ai"
	}
	// The usage API lives at the origin root (/alpha/...), not under the
	// provider API base path (e.g. /provider/v1), so collapse to scheme+host.
	if origin, err := url.Parse(base); err == nil && origin.Host != "" {
		base = origin.Scheme + "://" + origin.Host
	}
	headers := bearer(cred.Credential)
	headers["User-Agent"] = commandCodeUserAgent
	var payload struct {
		Credits *struct {
			MonthlyCredits   *float64 `json:"monthlyCredits"`
			PurchasedCredits *float64 `json:"purchasedCredits"`
			FreeCredits      *float64 `json:"freeCredits"`
		} `json:"credits"`
		WindowLimits *struct {
			Limited  bool               `json:"limited"`
			Exceeded *bool              `json:"exceeded"`
			FiveHour *commandCodeWindow `json:"fiveHour"`
			Weekly   *commandCodeWindow `json:"weekly"`
		} `json:"windowLimits"`
	}
	if err := getJSON(ctx, client, base+"/alpha/billing/credits", headers, &payload); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{}
	if wl := payload.WindowLimits; wl != nil {
		if w := commandCodeWindowView("5h", wl.FiveHour); w != nil {
			snap.Windows = append(snap.Windows, *w)
		}
		if w := commandCodeWindowView("weekly", wl.Weekly); w != nil {
			snap.Windows = append(snap.Windows, *w)
		}
	}
	// Monthly credits are the subscription pool. Show a monthly bar when the
	// plan reports a remaining balance and a period spend to anchor the cap.
	// Pay-as-you-go has no subscription detail (nil monthlyCredits), so no
	// monthly window is surfaced.
	if payload.Credits != nil && payload.Credits.MonthlyCredits != nil {
		if w := commandCodeMonthlyWindow(ctx, client, base, headers, *payload.Credits.MonthlyCredits); w != nil {
			snap.Windows = append(snap.Windows, *w)
		}
	}
	// The plan id is display-only; resolve it best-effort so a plan-endpoint
	// failure never hides the windows.
	snap.Plan = commandCodePlan(ctx, client, base, headers)
	return snap, nil
}

// commandCodeMonthlyWindow derives the monthly subscription pool bar. The
// remaining balance comes from the credits endpoint; the period spend comes
// from the usage summary. The cap is their sum (spend + remaining), so the bar
// is correct without a hardcoded plan-to-pool table. A usage-summary failure or
// a zero cap yields nil, so no misleading bar is shown.
func commandCodeMonthlyWindow(ctx context.Context, client *http.Client, base string, headers map[string]string, remaining float64) *Window {
	if remaining < 0 {
		remaining = 0
	}
	var payload struct {
		TotalMonthlyCredits *float64 `json:"totalMonthlyCredits"`
	}
	spend := 0.0
	haveSpend := false
	if err := getJSON(ctx, client, base+"/alpha/usage/summary", headers, &payload); err == nil && payload.TotalMonthlyCredits != nil {
		spend = max(0, *payload.TotalMonthlyCredits)
		haveSpend = true
	}
	// Without the period spend we cannot derive the pool cap, so there is no
	// meaningful bar; a remaining-only window would mislabel as unlimited.
	if !haveSpend {
		return nil
	}
	cap := spend + remaining
	if cap <= 0 {
		return nil
	}
	rem := remaining
	limit := cap
	return &Window{
		Label:       "monthly",
		UsedPercent: percentPtr(spend / cap * 100),
		Remaining:   &rem,
		Limit:       &limit,
	}
}

// commandCodeWindow is one rolling usage window. used and cap are credit-value
// units; resetAt is epoch milliseconds (0 means no active reset).
type commandCodeWindow struct {
	Used    float64 `json:"used"`
	Cap     float64 `json:"cap"`
	ResetAt int64   `json:"resetAt"`
}

func commandCodeWindowView(label string, w *commandCodeWindow) *Window {
	if w == nil {
		return nil
	}
	out := Window{Label: label}
	if w.Cap > 0 {
		out.UsedPercent = percentPtr(w.Used / w.Cap * 100)
		cap := w.Cap
		remaining := max(0, w.Cap-w.Used)
		out.Limit = &cap
		out.Remaining = &remaining
	}
	if w.ResetAt > 0 {
		t := time.UnixMilli(w.ResetAt).UTC()
		out.ResetsAt = &t
	}
	return &out
}

// commandCodePlan fetches the subscription plan id (e.g. "individual-goat").
// Any failure returns an empty string so the caller still reports the windows.
func commandCodePlan(ctx context.Context, client *http.Client, base string, headers map[string]string) string {
	var payload struct {
		Data *struct {
			PlanID string `json:"planId"`
		} `json:"data"`
	}
	if err := getJSON(ctx, client, base+"/alpha/billing/subscriptions", headers, &payload); err != nil {
		return ""
	}
	if payload.Data == nil {
		return ""
	}
	return payload.Data.PlanID
}
