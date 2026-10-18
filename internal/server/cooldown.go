package server

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// cooldownEntry is the per-target fallback cooldown snapshot. Besides the
// window itself it retains the origin of the failure that opened the cooldown
// (the failing request's client request id and error) so the admin UI can
// explain why a target is currently cooled without a DB lookup.
type cooldownEntry struct {
	providerModelID    string
	until              time.Time
	startedAt          time.Time
	provider           string
	model              string
	originRequestLogID string
	originErrorClass   string
	originErrorMessage string
}

type cooldownStore struct {
	mu    sync.Mutex
	until map[string]cooldownEntry
}

func newCooldownStore() *cooldownStore {
	return &cooldownStore{until: map[string]cooldownEntry{}}
}

// cooled reports whether the given target is still cooling. The read path does
// not prune: it treats an expired entry as not-cooled anyway, and cooling is
// checked once per candidate on every ordered-fallback request, so a full map
// scan here would put O(candidates x map size) work on the latency-sensitive
// routing path. Expired entries are dropped by set() and snapshot() instead,
// and their memory is bounded by cooldown churn.
func (c *cooldownStore) cooled(accountID, id string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.until[tenantKey(accountID, id)]
	if !ok {
		return false
	}
	return now.Before(e.until)
}

// pruneExpiredLocked drops entries whose window has closed. Without it the map
// only ever grows: an entry was otherwise removed only when a later request to
// the same target succeeded or an admin released it. Caller holds c.mu.
func (c *cooldownStore) pruneExpiredLocked(now time.Time) {
	for key, e := range c.until {
		if !now.Before(e.until) {
			delete(c.until, key)
		}
	}
}

// set records a cooldown window for the given provider_model_id. startedAt is
// the failure moment, until when the target becomes eligible again. The origin
// fields describe the failure that opened the cooldown.
func (c *cooldownStore) set(accountID, id string, startedAt, until time.Time, provider, model, originRequestLogID, originErrorClass, originErrorMessage string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneExpiredLocked(startedAt)
	key := tenantKey(accountID, id)
	// A cooldown only ever extends: a failure that would schedule a shorter
	// window than the one already open must not release the target early. The
	// longer window wins; ties keep the newer origin detail.
	if existing, ok := c.until[key]; ok && existing.until.After(until) {
		return
	}
	c.until[key] = cooldownEntry{
		providerModelID:    id,
		startedAt:          startedAt,
		until:              until,
		provider:           provider,
		model:              model,
		originRequestLogID: originRequestLogID,
		originErrorClass:   originErrorClass,
		originErrorMessage: originErrorMessage,
	}
}

// deleteLocked removes a cooldown entry. Caller holds c.mu.
func (c *cooldownStore) deleteLocked(key string) {
	delete(c.until, key)
}

// statusByName returns the live cooldown entry for the given provider/model
// pair if it is still cooling at now, and false otherwise. The store is keyed
// by provider_model_id but the UI addresses targets by names; when two model
// rows share a provider+model name the earliest expiry wins so the answer is
// deterministic rather than map-order dependent.
func (c *cooldownStore) statusByName(accountID, provider, model string, now time.Time) (cooldownEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var best cooldownEntry
	found := false
	prefix := accountID + "\x00"
	for key, e := range c.until {
		if !strings.HasPrefix(key, prefix) || e.provider != provider || e.model != model || !now.Before(e.until) {
			continue
		}
		if !found || e.until.Before(best.until) || (e.until.Equal(best.until) && e.providerModelID < best.providerModelID) {
			best, found = e, true
		}
	}
	return best, found
}

func (c *cooldownStore) clearFor(accountID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := accountID + "\x00"
	for key := range c.until {
		if strings.HasPrefix(key, prefix) {
			delete(c.until, key)
		}
	}
}

func (c *cooldownStore) remove(accountID, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteLocked(tenantKey(accountID, id))
}

// removeByName deletes every cooldown entry matching the given provider/model
// names. The store is keyed by provider_model_id but the admin UI addresses
// targets by names, so a manual release by name clears all rows that share it.
// It reports whether an entry was actually removed.
func (c *cooldownStore) removeByName(accountID, provider, model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := false
	prefix := accountID + "\x00"
	for key, entry := range c.until {
		if !strings.HasPrefix(key, prefix) || entry.provider != provider || entry.model != model {
			continue
		}
		delete(c.until, key)
		removed = true
	}
	return removed
}

// cooldownView is the per-target cooldown state surfaced to the admin/live UI.
type cooldownView struct {
	ProviderModelID    string `json:"provider_model_id"`
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	UntilAt            string `json:"until_at"`
	OriginErrorClass   string `json:"origin_error_class"`
	OriginErrorMessage string `json:"origin_error_message"`
}

// snapshot returns a copy of all currently-cooling cooldown windows keyed by
// provider_model_id. Expired windows are omitted.
func (c *cooldownStore) snapshot(accountID string, now time.Time) map[string]cooldownView {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneExpiredLocked(now)
	out := make(map[string]cooldownView, len(c.until))
	prefix := accountID + "\x00"
	for key, e := range c.until {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if !now.Before(e.until) {
			continue
		}
		out[e.providerModelID] = cooldownView{
			ProviderModelID:    e.providerModelID,
			Provider:           e.provider,
			Model:              e.model,
			UntilAt:            e.until.Format(time.RFC3339Nano),
			OriginErrorClass:   e.originErrorClass,
			OriginErrorMessage: e.originErrorMessage,
		}
	}
	return out
}

// cooldownStatus reports the live cooldown state for the provider/model given
// as ?provider=<name>&model=<upstream>. It is keyed by the names stored on the
// in-memory entry so the admin UI can interrogate a specific target's cooldown
// without persisting provider_model_id to the activity tables. Timing is
// computed at request time, so "restored in" always reflects current state.
func (s *Server) cooldownStatus(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	model := r.URL.Query().Get("model")
	if provider == "" || model == "" {
		adminError(w, http.StatusBadRequest, "invalid_request", "provider and model query parameters are required.")
		return
	}
	now := time.Now()
	entry, ok := s.cooldown.statusByName(s.scope(r).AccountID(), provider, model, now)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"cooling":              false,
			"provider":             provider,
			"model":                model,
			"started_at":           nil,
			"until_at":             nil,
			"ago_seconds":          0,
			"restored_in_seconds":  0,
			"origin_request_id":    "",
			"origin_error_class":   "",
			"origin_error_message": "",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cooling":              true,
		"provider":             entry.provider,
		"model":                entry.model,
		"started_at":           entry.startedAt.Format(time.RFC3339Nano),
		"until_at":             entry.until.Format(time.RFC3339Nano),
		"ago_seconds":          int64(now.Sub(entry.startedAt).Seconds()),
		"restored_in_seconds":  int64(entry.until.Sub(now).Seconds()),
		"origin_request_id":    entry.originRequestLogID,
		"origin_error_class":   entry.originErrorClass,
		"origin_error_message": entry.originErrorMessage,
	})
}

// clearCooldown manually removes the cooldown window for the provider/model
// given as ?provider=<name>&model=<upstream>, so an admin can immediately
// re-allow a target that is still cooling. It is a no-op (204) when the target
// is not currently in cooldown.
func (s *Server) clearCooldown(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	model := r.URL.Query().Get("model")
	if provider == "" || model == "" {
		adminError(w, http.StatusBadRequest, "invalid_request", "provider and model query parameters are required.")
		return
	}
	s.cooldown.removeByName(s.scope(r).AccountID(), provider, model)
	w.WriteHeader(http.StatusNoContent)
}
