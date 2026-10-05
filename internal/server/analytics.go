package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/store"
)

// Analytics provider identifiers. "custom" is any operator-supplied https
// script URL with no provider-specific data attribute.
const (
	analyticsProviderUmami     = "umami"
	analyticsProviderPlausible = "plausible"
	analyticsProviderCustom    = "custom"

	maxAnalyticsScriptURLLen = 2048
	maxAnalyticsSiteIDLen    = 256
)

func validAnalyticsProvider(provider string) bool {
	switch provider {
	case analyticsProviderUmami, analyticsProviderPlausible, analyticsProviderCustom:
		return true
	default:
		return false
	}
}

// analyticsScriptOrigin returns the CSP origin (scheme + host + optional port)
// for an operator-supplied script URL. It requires an absolute https URL with
// no embedded credentials; anything else is rejected so the operator cannot
// smuggle a non-TLS or userinfo-bearing origin into the policy.
func analyticsScriptOrigin(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil {
		return "", false
	}
	return "https://" + parsed.Host, true
}

// validateAnalyticsSettings returns a human-readable reason the proposal is
// invalid, or "" when it is acceptable. A disabled integration never requires
// a provider or URL, but length limits still apply so stored values stay
// bounded.
func validateAnalyticsSettings(settings store.PlatformAnalyticsSettings) string {
	if len(settings.ScriptURL) > maxAnalyticsScriptURLLen {
		return "Analytics script URL is too long."
	}
	if len(settings.SiteID) > maxAnalyticsSiteIDLen {
		return "Analytics site ID is too long."
	}
	if !settings.Enabled {
		return ""
	}
	if !validAnalyticsProvider(settings.Provider) {
		return "Choose a supported analytics provider."
	}
	if _, ok := analyticsScriptOrigin(settings.ScriptURL); !ok {
		return "Analytics script URL must be an absolute https URL."
	}
	if settings.Provider != analyticsProviderCustom && strings.TrimSpace(settings.SiteID) == "" {
		return "A site ID is required for the selected analytics provider."
	}
	return ""
}

// setPlatformAnalytics replaces the in-memory analytics cache that feeds the
// per-response CSP.
func (s *Server) setPlatformAnalytics(settings store.PlatformAnalyticsSettings) {
	s.platformAnalyticsMu.Lock()
	s.platformAnalytics = settings
	s.platformAnalyticsMu.Unlock()
}

// analyticsScriptOrigin returns the cached analytics script origin for the
// current hosted deployment, or ok=false when analytics is disabled or not
// configured. It is called on every response, so it must stay allocation-free
// on the common (disabled) path.
func (s *Server) analyticsScriptOrigin() (string, bool) {
	if s.config.Mode != config.ModeHosted {
		return "", false
	}
	s.platformAnalyticsMu.RLock()
	settings := s.platformAnalytics
	s.platformAnalyticsMu.RUnlock()
	if !settings.Enabled {
		return "", false
	}
	return analyticsScriptOrigin(settings.ScriptURL)
}

// analyticsOptions serves the public, non-secret analytics configuration to
// the consent-gated frontend. The script is never loaded by the server; the
// browser requests it only after the visitor consents.
func (s *Server) analyticsOptions(w http.ResponseWriter, r *http.Request) {
	settings, err := s.storeHandle().GetPlatformAnalyticsSettings(r.Context())
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "analytics_unavailable", "Analytics options are temporarily unavailable.")
		return
	}
	if !settings.Enabled {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":    true,
		"provider":   settings.Provider,
		"script_url": settings.ScriptURL,
		"site_id":    settings.SiteID,
	})
}
