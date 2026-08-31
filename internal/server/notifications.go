package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

// Notification event identifiers. These are stable, machine-readable values
// used in the outbound payload's "event" field.
const (
	eventFallback   = "virtual_model_fallback"
	eventAllFailed  = "virtual_model_failed"
	eventTest       = "test"
	severityWarning = "warning"
	severityError   = "error"
)

// notificationTimeout bounds a single best-effort webhook delivery. Delivery
// is fire-and-forget and never blocks or delays the inference request.
const notificationTimeout = 5 * time.Second

// notificationPayload is the stable, metadata-only JSON body sent to the
// webhook. It shares the Activity privacy boundary: it never contains prompts,
// responses, tool arguments/results, reasoning, provider keys, client key
// plaintext, auth headers, cookies, or credentials.
type notificationPayload struct {
	Event          string `json:"event"`
	Severity       string `json:"severity"`
	Timestamp      string `json:"timestamp"`
	ClientKey      string `json:"client_key"`
	RequestedModel string `json:"requested_model"`
	VirtualModel   string `json:"virtual_model,omitempty"`
	FromProvider   string `json:"from_provider,omitempty"`
	FromModel      string `json:"from_model,omitempty"`
	ToProvider     string `json:"to_provider,omitempty"`
	ToModel        string `json:"to_model,omitempty"`
	Reason         string `json:"reason,omitempty"`
	AttemptCount   int    `json:"attempt_count"`
	FinalStatus    int    `json:"final_status"`
	Summary        string `json:"summary"`
}

// maybeNotify emits a single logical notification for a routed request based on
// its final outcome. The payload is built synchronously here (before the
// goroutine) because the caller's logRow continues to be mutated after this
// call; finalStatus is the client-facing status the request will return, which
// is not yet recorded on the row. Only the delivery runs detached, so it never
// blocks or alters the client response.
func (s *Server) maybeNotify(row *logRow, route resolvedRoute, resp *http.Response, finalStatus int) {
	var event string
	switch {
	case resp != nil && row.fallbackUsed:
		event = eventFallback
	case resp == nil && route.Virtual && hasFailedAttempt(row.attempts):
		event = eventAllFailed
	default:
		return
	}
	payload := s.buildNotificationPayload(event, row, route, finalStatus)
	go s.deliverNotification(event, payload)
}

// hasFailedAttempt reports whether any target was actually attempted upstream
// and failed (as opposed to merely being skipped as unavailable). This keeps
// "all targets failed" from firing when every target was skipped without an
// upstream attempt.
func hasFailedAttempt(attempts []requestAttempt) bool {
	for _, a := range attempts {
		if a.result == "failed" {
			return true
		}
	}
	return false
}

// attemptCount counts actual upstream attempts, excluding targets that were
// skipped without an upstream attempt (unavailable or protocol-mismatch). This
// is what "N targets attempted" in the payload summary means.
func attemptCount(attempts []requestAttempt) int {
	n := 0
	for _, a := range attempts {
		if a.result != "skipped" {
			n++
		}
	}
	return n
}

// deliverNotification loads the current notification config and, if the event
// is enabled, sends one best-effort webhook POST. Any failure is logged in
// normal admin diagnostics and never affects the inference request. The payload
// must already be built (it is a value, so it is immune to further row mutation).
func (s *Server) deliverNotification(event string, payload notificationPayload) {
	ctx := context.Background()
	cfg, err := s.db.GetNotificationSettings(ctx)
	if err != nil || !cfg.Enabled || cfg.WebhookURL == "" {
		return
	}
	if event == eventFallback && !cfg.EventFallback {
		return
	}
	if event == eventAllFailed && !cfg.EventAllFailed {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		s.logger.Warn("notification payload marshal failed", "event", event, "error", err.Error())
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		s.logger.Warn("notification request build failed", "event", event, "error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Tiller-Router/1")
	if cfg.AuthHeader != "" {
		req.Header.Set("Authorization", cfg.AuthHeader)
	}
	resp, err := s.notifyClient.Do(req)
	if err != nil {
		s.logger.Warn("notification delivery failed", "event", event, "error", err.Error())
		return
	}
	_ = resp.Body.Close()
}

// buildNotificationPayload assembles the metadata-only payload for an event
// from the request's routing outcome. Only metadata already recorded for
// Activity is used; no request or response content is ever included. It runs
// synchronously in the request goroutine (before delivery is spawned).
// finalStatus is the client-facing status the request will return (the row's
// own httpStatus is not always set yet at the call site).
func (s *Server) buildNotificationPayload(event string, row *logRow, route resolvedRoute, finalStatus int) notificationPayload {
	p := notificationPayload{
		Event:          event,
		Timestamp:      row.createdAt,
		ClientKey:      s.clientKeyName(context.Background(), row.clientKeyID),
		RequestedModel: row.requestedModel,
		VirtualModel:   route.RouteModel,
		AttemptCount:   attemptCount(row.attempts),
		FinalStatus:    finalStatus,
	}
	switch event {
	case eventFallback:
		p.Severity = severityWarning
		if from, ok := lastFailedAttempt(row.attempts); ok {
			p.FromProvider, p.FromModel = from.provider, from.model
		}
		if to, ok := successAttempt(row.attempts); ok {
			p.ToProvider, p.ToModel = to.provider, to.model
		}
		if row.fallbackReason != nil {
			p.Reason = *row.fallbackReason
		}
		p.Summary = fmt.Sprintf("Tiller fallback: %s — %s/%s → %s/%s", route.RouteModel, p.FromProvider, p.FromModel, p.ToProvider, p.ToModel)
	case eventAllFailed:
		p.Severity = severityError
		if row.fallbackReason != nil {
			p.Reason = *row.fallbackReason
		}
		p.Summary = fmt.Sprintf("Tiller routing failed: %s — %d targets attempted, no target succeeded", route.RouteModel, p.AttemptCount)
	case eventTest:
		p.Severity = severityWarning
		p.Summary = "Tiller test notification"
	}
	return p
}

// clientKeyName resolves a client key's display name. It is best-effort; an
// empty name is acceptable in a notification payload.
func (s *Server) clientKeyName(ctx context.Context, id string) string {
	var name string
	_ = s.db.SQL.QueryRowContext(ctx, `SELECT name FROM client_keys WHERE id=?`, id).Scan(&name)
	return name
}

// lastFailedAttempt returns the most recent upstream attempt that failed.
func lastFailedAttempt(attempts []requestAttempt) (requestAttempt, bool) {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].result == "failed" {
			return attempts[i], true
		}
	}
	return requestAttempt{}, false
}

// successAttempt returns the most recent upstream attempt that succeeded.
func successAttempt(attempts []requestAttempt) (requestAttempt, bool) {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].result == "success" {
			return attempts[i], true
		}
	}
	return requestAttempt{}, false
}

// sendTestNotification sends a harmless "Tiller test notification" to the
// configured webhook and reports success/failure to the administrator. It uses
// the saved configuration (URL + optional auth header) regardless of the
// enabled flag so an admin can verify delivery before enabling events.
func (s *Server) sendTestNotification(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetNotificationSettings(r.Context())
	if err != nil {
		adminError(w, 500, "database_error", "Could not load notification settings.")
		return
	}
	if cfg.WebhookURL == "" {
		adminError(w, 400, "no_webhook_url", "Configure a webhook URL before sending a test notification.")
		return
	}
	payload := notificationPayload{
		Event:     eventTest,
		Severity:  severityWarning,
		Timestamp: database.Now(),
		Summary:   "Tiller test notification",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		adminError(w, 500, "internal_error", "Could not build the test notification.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), notificationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		adminError(w, 400, "invalid_webhook_url", "The configured webhook URL is invalid.")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Tiller-Router/1")
	if cfg.AuthHeader != "" {
		req.Header.Set("Authorization", cfg.AuthHeader)
	}
	resp, err := s.notifyClient.Do(req)
	if err != nil {
		adminError(w, 502, "delivery_failed", "The test notification could not be delivered: "+err.Error())
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		adminError(w, 502, "delivery_failed", fmt.Sprintf("The webhook returned HTTP %d.", resp.StatusCode))
		return
	}
	writeJSON(w, 200, map[string]any{"delivered": true})
}
