package providerquota

import (
	"context"
	"net/http"
	"net/url"
	"strings"
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

// fetchOllamaCloud reads Ollama Cloud usage. The endpoint is undocumented but
// used by several clients; it reports session and weekly usage percentages. A
// failure is surfaced as unavailable, never fatal.
func fetchOllamaCloud(ctx context.Context, client *http.Client, cred Credential) (Snapshot, error) {
	var payload struct {
		Session *float64 `json:"session"`
		Weekly  *float64 `json:"weekly"`
		Limits  []struct {
			Label       string   `json:"label"`
			Description string   `json:"description"`
			UsedPercent *float64 `json:"used_percent"`
		} `json:"limits"`
	}
	url := "https://ollama.com/api/usage"
	if err := getJSON(ctx, client, url, bearer(cred.Credential), &payload); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{}
	if payload.Session != nil {
		snap.Windows = append(snap.Windows, Window{Label: "session", UsedPercent: percentPtr(*payload.Session)})
	}
	if payload.Weekly != nil {
		snap.Windows = append(snap.Windows, Window{Label: "weekly", UsedPercent: percentPtr(*payload.Weekly)})
	}
	for _, l := range payload.Limits {
		if l.UsedPercent == nil {
			continue
		}
		label := l.Label
		if label == "" {
			label = l.Description
		}
		snap.Windows = append(snap.Windows, Window{Label: label, UsedPercent: percentPtr(*l.UsedPercent)})
	}
	return snap, nil
}
