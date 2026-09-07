package server

import (
	"net/http"
	"sync"
	"time"
)

// cooldownEntry is the per-target fallback cooldown snapshot. Besides the
// window itself it retains the origin of the failure that opened the cooldown
// (the failing request's client request id and error) so the admin UI can
// explain why a target is currently cooled without a DB lookup.
type cooldownEntry struct {
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

func (c *cooldownStore) cooled(id string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.until[id]
	if !ok {
		return false
	}
	return now.Before(e.until)
}

// set records a cooldown window for the given provider_model_id. startedAt is
// the failure moment, until when the target becomes eligible again. The origin
// fields describe the failure that opened the cooldown.
func (c *cooldownStore) set(id string, startedAt, until time.Time, provider, model, originRequestLogID, originErrorClass, originErrorMessage string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until[id] = cooldownEntry{
		startedAt:          startedAt,
		until:              until,
		provider:           provider,
		model:              model,
		originRequestLogID: originRequestLogID,
		originErrorClass:   originErrorClass,
		originErrorMessage: originErrorMessage,
	}
}

// statusByName returns the live cooldown entry for the given provider/model
// pair if it is still cooling at now, and false otherwise. The store is keyed
// by provider_model_id but the UI addresses targets by names, so this does a
// linear match.
func (c *cooldownStore) statusByName(provider, model string, now time.Time) (cooldownEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.until {
		if e.provider == provider && e.model == model {
			if now.Before(e.until) {
				return e, true
			}
			return cooldownEntry{}, false
		}
	}
	return cooldownEntry{}, false
}

func (c *cooldownStore) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until = map[string]cooldownEntry{}
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
	entry, ok := s.cooldown.statusByName(provider, model, now)
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
