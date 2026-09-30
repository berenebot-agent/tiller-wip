package providerquota

import (
	"context"
	"encoding/json"
	"math"
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
// used by several clients. The verified shape is:
//
//	{"limits":{"session":{"usage":0.42},"weekly":{"usage":0.55}}}
//
// where usage is a fraction (0-1), so it is multiplied by 100. The parser is
// deliberately tolerant: it also accepts a top-level session/weekly fraction
// and a legacy array-of-limits form, so a future shape change degrades to
// "no quota reported" rather than a hard failure. The URL is built from the
// provider's base URL when set (defaulting to ollama.com) so tests can point
// it at a mock server.
func fetchOllamaCloud(ctx context.Context, client *http.Client, cred Credential) (Snapshot, error) {
	base := strings.TrimRight(cred.BaseURL, "/")
	if base == "" || strings.Contains(base, "host.docker.internal") {
		base = "https://ollama.com"
	}
	if origin, err := url.Parse(base); err == nil && origin.Host != "" {
		base = origin.Scheme + "://" + origin.Host
	}
	var payload struct {
		// Top-level fraction form (fallback).
		Session *float64 `json:"session"`
		Weekly  *float64 `json:"weekly"`
		// limits is decoded loosely because the verified shape is an object
		// ({"session":{"usage":0.42},...}) while older/alternative clients have
		// seen an array form. Decoding into RawMessage lets both be tried.
		Limits json.RawMessage `json:"limits"`
	}
	url := base + "/api/usage"
	if err := getJSON(ctx, client, url, bearer(cred.Credential), &payload); err != nil {
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

// ollamaUsage is one nested limit entry in the Ollama usage response. usage is
// a fraction (0-1).
type ollamaUsage struct {
	Usage *float64 `json:"usage"`
}
