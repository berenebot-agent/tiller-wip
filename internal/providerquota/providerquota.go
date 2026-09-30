// Package providerquota polls subscription/quota endpoints for providers whose
// usage is not visible from response token counts alone (Codex subscription,
// Claude Code subscription, GitHub Copilot, Z.ai GLM coding plan, Ollama
// Cloud, Command Code plans). It returns read-only, best-effort snapshots for
// admin display.
//
// The poller never blocks routing: a failed poll yields an "unavailable"
// snapshot and the request path is untouched. Snapshots are cached per
// account+provider and refreshed on the cadence the server selects (30 min
// background floor, 60 s while the provider is active, immediate-on-view with a
// 30 s floor). See docs/roadmap_usage_cost_quota.md.
package providerquota

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// maxBody bounds a quota endpoint response so a hostile or broken endpoint
// cannot exhaust memory. Quota payloads are small.
const maxBody = 1 << 20

// Snapshot is a provider's current subscription/quota state. All fields are
// best-effort; a poll failure yields Available=false with an optional Reason.
type Snapshot struct {
	ProviderID   string    `json:"provider_id"`
	ProviderName string    `json:"provider_name,omitempty"`
	ProviderType string    `json:"provider_type,omitempty"`
	Plan         string    `json:"plan,omitempty"`
	Available    bool      `json:"available"`
	Reason       string    `json:"reason,omitempty"`
	Windows      []Window  `json:"windows,omitempty"`
	FetchedAt    time.Time `json:"fetched_at,omitempty"`
}

// Window is one quota window (e.g. a 5-hour or weekly limit). UsedPercent is
// 0-100 when known; Remaining is a provider-native remaining amount when the
// provider reports one. ResetsAt is an absolute reset time when known.
type Window struct {
	Label       string     `json:"label"`
	UsedPercent *float64   `json:"used_percent,omitempty"`
	Remaining   *float64   `json:"remaining,omitempty"`
	Limit       *float64   `json:"limit,omitempty"`
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
}

// Credential is the material a quota adapter needs. Credential is the hydrated
// OAuth access token (or API key) for the provider instance.
type Credential struct {
	AccountID  string
	ProviderID string
	Name       string
	Type       string
	BaseURL    string
	Credential string
	// AccountIDHeader carries a provider-account id when the adapter needs it
	// (e.g. ChatGPT account id for Codex usage).
	AccountIDHeader string
}

// Adapter polls one provider type's quota endpoint.
type Adapter func(ctx context.Context, client *http.Client, cred Credential) (Snapshot, error)

// adapters maps a provider type to its quota adapter. Providers without an
// adapter are simply not polled.
var adapters = map[string]Adapter{
	"codex-subscription":  fetchCodex,
	"claude-subscription": fetchClaude,
	"github-copilot":      fetchCopilot,
	"zai":                 fetchZAI,
	"ollama-cloud":        fetchOllamaCloud,
	"commandcode":         fetchCommandCode,
}

// Supports reports whether a provider type has a quota adapter.
func Supports(providerType string) bool {
	_, ok := adapters[providerType]
	return ok
}

// getJSON performs a bounded GET with the given headers and decodes JSON into
// target. A non-2xx status is an error.
func getJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{status: resp.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(target)
}

type statusError struct{ status int }

func (e *statusError) Error() string { return http.StatusText(e.status) }

// bearer returns a bearer Authorization header map.
func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// percentPtr returns a pointer to a clamped 0-100 percentage.
func percentPtr(v float64) *float64 {
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return &v
}
