package providerquota

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// fetchZAI reads the GLM coding-plan quota. The endpoint returns a list of
// limits, each with a type (TOKENS_LIMIT / CREDIT_LIMIT / TIME_LIMIT), a
// percentage and window metadata. Only the reported values are surfaced; no
// timezone correction is invented for reset times.
func fetchZAI(ctx context.Context, client *http.Client, cred Credential) (Snapshot, error) {
	base := strings.TrimRight(cred.BaseURL, "/")
	if base == "" {
		base = "https://api.z.ai"
	}
	if origin, err := url.Parse(base); err == nil && origin.Host != "" {
		base = origin.Scheme + "://" + origin.Host
	}
	url := base + "/api/monitor/usage/quota/limit"
	var payload struct {
		Success bool `json:"success"`
		Code    int  `json:"code"`
		Data    *struct {
			PlanName string `json:"planName"`
			Plan     string `json:"plan"`
			Limits   []struct {
				Type         string   `json:"type"`
				Unit         int      `json:"unit"`
				Number       int      `json:"number"`
				Percentage   float64  `json:"percentage"`
				Usage        *float64 `json:"usage"`
				Remaining    *float64 `json:"remaining"`
				CurrentValue *float64 `json:"currentValue"`
			} `json:"limits"`
		} `json:"data"`
	}
	if err := getJSON(ctx, client, url, bearer(cred.Credential), &payload); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{}
	if payload.Data == nil {
		return snap, nil
	}
	if payload.Data.PlanName != "" {
		snap.Plan = payload.Data.PlanName
	} else {
		snap.Plan = payload.Data.Plan
	}
	for _, l := range payload.Data.Limits {
		if l.Type != "TOKENS_LIMIT" && l.Type != "CREDIT_LIMIT" {
			continue
		}
		w := Window{Label: zaiWindowLabel(l.Unit, l.Number)}
		pct := l.Percentage
		if l.Usage != nil && *l.Usage > 0 {
			used := 0.0
			if l.Remaining != nil {
				used = *l.Usage - *l.Remaining
			} else if l.CurrentValue != nil {
				used = *l.CurrentValue
			}
			pct = used / *l.Usage * 100
		}
		w.UsedPercent = percentPtr(pct)
		if l.Remaining != nil {
			r := *l.Remaining
			w.Remaining = &r
		}
		if l.Usage != nil {
			u := *l.Usage
			w.Limit = &u
		}
		snap.Windows = append(snap.Windows, w)
	}
	return snap, nil
}

// zaiWindowLabel names a Z.ai limit window from its unit+number. Unit 5 is
// minutes, 3 hours, 1 days, 6 weeks per the provider's own scheme; an unknown
// combination falls back to a generic label.
func zaiWindowLabel(unit, number int) string {
	multipliers := map[int]int{1: 1440, 3: 60, 5: 1, 6: 10080}
	minutes := 0
	if m, ok := multipliers[unit]; ok {
		minutes = number * m
	}
	switch {
	case minutes == 300:
		return "5h"
	case minutes >= 10080:
		return "weekly"
	case minutes >= 1440:
		return "daily"
	case minutes >= 60:
		return "hourly"
	default:
		return "session"
	}
}

// fetchOllamaCloud reads Ollama Cloud usage. Ollama has shipped two response
// generations for its subscription windows, so the adapter probes both and
// merges whichever reports data (detection, not a fixed endpoint):
//
//   - Current: GET /api/balance returns
//     {"included":{"session":{"remaining_percent":72.3,"resets_at":...},
//     "weekly":{"remaining_percent":18.5,"resets_at":...}},
//     "purchased":{"balance_usd":0}}
//     where remaining_percent is already 0-100, so used = 100 - remaining.
//   - Older: GET /api/usage returned the window limits as a nested object
//     {"limits":{"session":{"usage":0.42},"weekly":{"usage":0.55}}}, a
//     top-level fraction {"session":0.1,"weekly":0.2}, or a legacy array
//     [{label,description,used_percent}]; usage there is a 0-1 fraction.
//
// Both requests are attempted; the first that yields windows wins, so an
// account on either build works. The parser is deliberately tolerant: an
// unrecognised shape degrades to "no quota reported" rather than a hard
// failure, and only a total failure of both endpoints is an error. The URL is
// built from the provider's base URL when set (defaulting to ollama.com) so
// tests can point it at a mock server.
func fetchOllamaCloud(ctx context.Context, client *http.Client, cred Credential) (Snapshot, error) {
	base := strings.TrimRight(cred.BaseURL, "/")
	if base == "" || strings.Contains(base, "host.docker.internal") {
		base = "https://ollama.com"
	}
	if origin, err := url.Parse(base); err == nil && origin.Host != "" {
		base = origin.Scheme + "://" + origin.Host
	}
	headers := bearer(cred.Credential)

	// Current generation: /api/balance reports remaining percentages. A missing
	// or erroring endpoint is not fatal — fall through and try the old form.
	balanceSnap, balanceErr := fetchOllamaBalance(ctx, client, base+"/api/balance", headers)
	if balanceErr == nil && len(balanceSnap.Windows) > 0 {
		return balanceSnap, nil
	}
	// Older generation: /api/usage reports session/weekly usage fractions. Its
	// result (even empty) is returned when it succeeds, so an unrecognised
	// shape degrades to "no quota reported" rather than an error.
	usageSnap, usageErr := fetchOllamaUsage(ctx, client, base+"/api/usage", headers)
	if usageErr == nil {
		return usageSnap, nil
	}
	// /api/usage failed. If /api/balance at least responded, prefer its result
	// (empty → "format unavailable") over reporting the unavailable endpoint.
	if balanceErr == nil {
		return balanceSnap, nil
	}
	return Snapshot{}, usageErr
}

// fetchOllamaBalance decodes the /api/balance response. remaining_percent is a
// 0-100 figure, so the used percentage is its complement. The purchased
// balance, when non-zero, is surfaced as a "credits" window in USD.
func fetchOllamaBalance(ctx context.Context, client *http.Client, url string, headers map[string]string) (Snapshot, error) {
	var payload struct {
		Included *struct {
			Session *ollamaBalanceWindow `json:"session"`
			Weekly  *ollamaBalanceWindow `json:"weekly"`
		} `json:"included"`
		Purchased *struct {
			BalanceUSD *float64 `json:"balance_usd"`
		} `json:"purchased"`
	}
	if err := getJSON(ctx, client, url, headers, &payload); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{}
	add := func(label string, w *ollamaBalanceWindow) {
		if w == nil || w.RemainingPercent == nil {
			return
		}
		out := Window{Label: label, UsedPercent: percentPtr(100 - *w.RemainingPercent)}
		if w.ResetsAt != "" {
			if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
				u := t.UTC()
				out.ResetsAt = &u
			}
		}
		snap.Windows = append(snap.Windows, out)
	}
	if payload.Included != nil {
		add("session", payload.Included.Session)
		add("weekly", payload.Included.Weekly)
	}
	if payload.Purchased != nil && payload.Purchased.BalanceUSD != nil && *payload.Purchased.BalanceUSD > 0 {
		// Purchased credit balance is a remaining amount, not a percentage.
		// Surface it as a "credits" window with only a Remaining figure so the
		// UI renders "$ left" instead of a bogus percentage.
		bal := *payload.Purchased.BalanceUSD
		snap.Windows = append(snap.Windows, Window{Label: "credits", Remaining: &bal})
	}
	return snap, nil
}

// fetchOllamaUsage decodes the older /api/usage response shapes: a limits
// object with nested usage fractions, a top-level session/weekly fraction, or a
// legacy array of {label,used_percent}. usage is a 0-1 fraction here.
func fetchOllamaUsage(ctx context.Context, client *http.Client, url string, headers map[string]string) (Snapshot, error) {
	var payload struct {
		// Top-level fraction form (fallback).
		Session *float64 `json:"session"`
		Weekly  *float64 `json:"weekly"`
		// limits is decoded loosely because the verified shape is an object
		// ({"session":{"usage":0.42},...}) while older/alternative clients have
		// seen an array form. Decoding into RawMessage lets both be tried.
		Limits json.RawMessage `json:"limits"`
	}
	if err := getJSON(ctx, client, url, headers, &payload); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{}
	addFraction := func(label string, v *float64) {
		if v == nil {
			return
		}
		// usage is a fraction of the quota (0-1); scale to a percentage and
		// round to avoid float noise like 55.00000000000001.
		pct := math.Round(*v*1000) / 10
		snap.Windows = append(snap.Windows, Window{Label: label, UsedPercent: percentPtr(pct)})
	}
	addFraction("session", payload.Session)
	addFraction("weekly", payload.Weekly)
	if len(payload.Limits) > 0 {
		// Verified object form first.
		var obj struct {
			Session *ollamaUsage `json:"session"`
			Weekly  *ollamaUsage `json:"weekly"`
		}
		if err := json.Unmarshal(payload.Limits, &obj); err == nil && (obj.Session != nil || obj.Weekly != nil) {
			if obj.Session != nil {
				addFraction("session", obj.Session.Usage)
			}
			if obj.Weekly != nil {
				addFraction("weekly", obj.Weekly.Usage)
			}
		} else {
			// Legacy array form: [{label,description,used_percent}].
			var arr []struct {
				Label       string   `json:"label"`
				Description string   `json:"description"`
				UsedPercent *float64 `json:"used_percent"`
			}
			if err := json.Unmarshal(payload.Limits, &arr); err == nil {
				for _, l := range arr {
					if l.UsedPercent == nil {
						continue
					}
					label := l.Label
					if label == "" {
						label = l.Description
					}
					snap.Windows = append(snap.Windows, Window{Label: label, UsedPercent: percentPtr(*l.UsedPercent)})
				}
			}
		}
	}
	return snap, nil
}

// ollamaBalanceWindow is one window in the /api/balance "included" object.
// remaining_percent is already 0-100.
type ollamaBalanceWindow struct {
	RemainingPercent *float64 `json:"remaining_percent"`
	ResetsAt         string   `json:"resets_at"`
}

// ollamaUsage is one nested limit entry in the Ollama usage response. usage is
// a fraction (0-1).
type ollamaUsage struct {
	Usage *float64 `json:"usage"`
}
