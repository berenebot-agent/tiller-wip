package providerquota

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// fetchCommandCode reads the Command Code plan's rolling usage windows. Like
// Ollama Cloud and the Codex/Claude adapters, this endpoint is undocumented in
// the public Provider API docs; it is the internal billing API the official CLI
// uses to render `/usage`. The verified shape (command-code CLI) is:
//
//	GET {origin}/alpha/billing/credits
//	{"credits":{"windowLimits":{"limited":true,
//	  "fiveHour":{"used":3.2,"cap":10,"resetAt":1730000000000},
//	  "weekly":{"used":8.0,"cap":35,"resetAt":1730000000000}},
//	  "purchasedRemaining":5,"freeRemaining":1}}
//
// resetAt is epoch milliseconds. The plan id is fetched separately from
// /alpha/billing/subscriptions and is best-effort: a failure there must not
// discard the windows. All values are credit-value units, not tokens.
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
	var payload struct {
		Credits *struct {
			WindowLimits *struct {
				Limited  bool               `json:"limited"`
				FiveHour *commandCodeWindow `json:"fiveHour"`
				Weekly   *commandCodeWindow `json:"weekly"`
			} `json:"windowLimits"`
			PurchasedRemaining *float64 `json:"purchasedRemaining"`
			FreeRemaining      *float64 `json:"freeRemaining"`
		} `json:"credits"`
	}
	if err := getJSON(ctx, client, base+"/alpha/billing/credits", headers, &payload); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{}
	if payload.Credits != nil {
		if wl := payload.Credits.WindowLimits; wl != nil {
			if w := commandCodeWindowView("5h", wl.FiveHour); w != nil {
				snap.Windows = append(snap.Windows, *w)
			}
			if w := commandCodeWindowView("weekly", wl.Weekly); w != nil {
				snap.Windows = append(snap.Windows, *w)
			}
		}
		// Extra credits (pay-as-you-go + free) are never capped. Surface the
		// remaining balance the same way the CLI does: purchased + free.
		if payload.Credits.PurchasedRemaining != nil || payload.Credits.FreeRemaining != nil {
			remaining := 0.0
			if payload.Credits.PurchasedRemaining != nil {
				remaining += *payload.Credits.PurchasedRemaining
			}
			if payload.Credits.FreeRemaining != nil {
				remaining += *payload.Credits.FreeRemaining
			}
			snap.Windows = append(snap.Windows, Window{Label: "credits", Remaining: &remaining})
		}
	}
	// The plan id is display-only; resolve it best-effort so a plan-endpoint
	// failure never hides the windows.
	snap.Plan = commandCodePlan(ctx, client, base, headers)
	return snap, nil
}

// commandCodeWindow is one rolling usage window. used and cap are credit-value
// units; resetAt is epoch milliseconds.
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

// commandCodePlan fetches the subscription plan id (e.g. "individual-pro").
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
