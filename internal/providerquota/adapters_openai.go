package providerquota

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// fetchCodex reads the ChatGPT/Codex subscription usage. The endpoint path
// differs by base-URL style: a `/backend-api` host uses `/wham/usage`, a Codex
// API host uses `/api/codex/usage`. The payload reports a primary and secondary
// rate-limit window with used_percent and a reset time, plus optional credits.
func fetchCodex(ctx context.Context, client *http.Client, cred Credential) (Snapshot, error) {
	base := strings.TrimRight(cred.BaseURL, "/")
	if base == "" {
		base = "https://chatgpt.com/backend-api/codex"
	}
	var urls []string
	if strings.Contains(base, "/backend-api") {
		base = strings.Split(base, "/backend-api")[0] + "/backend-api"
		urls = []string{base + "/wham/usage", base + "/codex/usage"}
	} else {
		urls = []string{base + "/api/codex/usage", base + "/codex/usage"}
	}
	headers := bearer(cred.Credential)
	headers["User-Agent"] = "codex_cli_rs"
	if cred.AccountIDHeader != "" {
		headers["ChatGPT-Account-Id"] = cred.AccountIDHeader
	}

	var payload struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			PrimaryWindow   *codexWindow `json:"primary_window"`
			SecondaryWindow *codexWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	var lastErr error
	for _, url := range urls {
		if err := getJSON(ctx, client, url, headers, &payload); err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return Snapshot{}, lastErr
	}
	snap := Snapshot{Plan: payload.PlanType}
	if payload.RateLimit != nil {
		if w := codexWindowView("5h", payload.RateLimit.PrimaryWindow); w != nil {
			snap.Windows = append(snap.Windows, *w)
		}
		if w := codexWindowView("weekly", payload.RateLimit.SecondaryWindow); w != nil {
			snap.Windows = append(snap.Windows, *w)
		}
	}
	return snap, nil
}

type codexWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int     `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

func codexWindowView(label string, w *codexWindow) *Window {
	if w == nil {
		return nil
	}
	out := Window{Label: label, UsedPercent: percentPtr(w.UsedPercent)}
	if w.ResetAt > 0 {
		t := time.Unix(w.ResetAt, 0).UTC()
		out.ResetsAt = &t
	}
	if w.LimitWindowSeconds > 0 {
		out.Label = windowLabel(w.LimitWindowSeconds, label)
	}
	return &out
}

// windowLabel names a window by its duration when the provider reports one.
func windowLabel(seconds int, fallback string) string {
	switch {
	case seconds >= 6*24*3600:
		return "weekly"
	case seconds >= 24*3600:
		return "daily"
	case seconds >= 4*3600 && seconds < 6*3600:
		return "5h"
	case seconds >= 3600:
		return "hourly"
	default:
		return fallback
	}
}

// fetchClaude reads the Claude Code OAuth subscription usage. The endpoint
// reports five_hour and seven_day utilization percentages with reset times and,
// optionally, extra usage.
func fetchClaude(ctx context.Context, client *http.Client, cred Credential) (Snapshot, error) {
	headers := bearer(cred.Credential)
	headers["anthropic-beta"] = "oauth-2025-04-20"
	headers["Accept"] = "application/json"
	var payload struct {
		FiveHour *claudeWindow `json:"five_hour"`
		SevenDay *claudeWindow `json:"seven_day"`
		Extra    *struct {
			IsEnabled bool    `json:"is_enabled"`
			Used      float64 `json:"used_credits"`
			Limit     float64 `json:"monthly_limit"`
		} `json:"extra_usage"`
	}
	if err := getJSON(ctx, client, "https://api.anthropic.com/api/oauth/usage", headers, &payload); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{}
	if w := claudeWindowView("5h", payload.FiveHour); w != nil {
		snap.Windows = append(snap.Windows, *w)
	}
	if w := claudeWindowView("7d", payload.SevenDay); w != nil {
		snap.Windows = append(snap.Windows, *w)
	}
	if payload.Extra != nil && payload.Extra.IsEnabled {
		used, limit := payload.Extra.Used, payload.Extra.Limit
		remaining := max(0, limit-used)
		w := Window{Label: "extra credits", Remaining: &remaining, Limit: &limit}
		if limit > 0 {
			w.UsedPercent = percentPtr(used / limit * 100)
		}
		snap.Windows = append(snap.Windows, w)
	}
	return snap, nil
}

type claudeWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

func claudeWindowView(label string, w *claudeWindow) *Window {
	if w == nil {
		return nil
	}
	out := Window{Label: label}
	if w.Utilization != nil {
		out.UsedPercent = percentPtr(*w.Utilization)
	}
	if w.ResetsAt != "" {
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
			u := t.UTC()
			out.ResetsAt = &u
		}
	}
	return &out
}

// fetchCopilot reads the GitHub Copilot entitlement, which reports quota
// snapshots per feature (chat, completions, premium requests) and the plan.
func fetchCopilot(ctx context.Context, client *http.Client, cred Credential) (Snapshot, error) {
	headers := bearer(cred.Credential)
	headers["Accept"] = "application/vnd.github+json"
	headers["X-GitHub-Api-Version"] = "2022-11-28"
	var payload struct {
		CopilotPlan    string `json:"copilot_plan"`
		QuotaSnapshots map[string]struct {
			Entitlement      float64  `json:"entitlement"`
			Remaining        float64  `json:"remaining"`
			PercentRemaining *float64 `json:"percent_remaining"`
		} `json:"quota_snapshots"`
	}
	if err := getJSON(ctx, client, "https://api.github.com/copilot_internal/user", headers, &payload); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Plan: payload.CopilotPlan}
	for name, q := range payload.QuotaSnapshots {
		w := Window{Label: name}
		remaining := q.Remaining
		limit := q.Entitlement
		w.Remaining = &remaining
		if limit > 0 {
			w.Limit = &limit
			w.UsedPercent = percentPtr((limit - remaining) / limit * 100)
		} else if q.PercentRemaining != nil {
			w.UsedPercent = percentPtr(100 - *q.PercentRemaining)
		}
		snap.Windows = append(snap.Windows, w)
	}
	return snap, nil
}
