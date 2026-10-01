package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/hostednet"
	"github.com/tiller-router/tiller-router/internal/store"
)

// Notification event identifiers. These are stable, machine-readable values
// used in the outbound payload's "event" field.
const (
	eventFallback         = "virtual_model_fallback"
	eventAllFailed        = "virtual_model_failed"
	eventTest             = "test"
	eventClientKeyCreated = "client_key_created"
	eventClientKeyDeleted = "client_key_deleted"
	eventAdminLogin       = "admin_login"
	severityWarning       = "warning"
	severityError         = "error"
	severityInfo          = "info"
)

// notificationTimeout bounds a single best-effort webhook delivery. Delivery
// is fire-and-forget and never blocks or delays the inference request.
const notificationTimeout = 5 * time.Second

const maxNotificationResponseBytes int64 = 64 << 10

// hostedNotificationCooldownSeconds is the pinned notification cooldown in
// hosted mode. A hosted customer can never perform an admin login (hosted
// accounts authenticate through the user session, and the admin-login event is
// only emitted by the local-mode admin session handler), so that event is
// disabled for them. The cooldown floor stops a tenant setting 0 and using the
// router as an unbounded webhook relay against arbitrary public HTTPS endpoints.
const hostedNotificationCooldownSeconds = 60

// hostedTestNotificationCooldown bounds how often a hosted account may trigger
// a manual test delivery. The test bypasses the notification cooldown (it is
// not subject to it), so without this a tenant could hammer any validated
// public HTTPS endpoint the router is willing to reach.
const hostedTestNotificationCooldown = 60 * time.Second

// notificationPayload is the metadata captured for a single routing event. It
// is rendered as a human-readable plain-text message for the webhook. It shares
// the Activity privacy boundary: it never contains prompts, responses, tool
// arguments/results, reasoning, provider keys, client key plaintext, auth
// headers, cookies, or credentials.
type notificationPayload struct {
	Event          string                `json:"event"`
	Severity       string                `json:"severity"`
	Timestamp      string                `json:"timestamp"`
	ClientKey      string                `json:"client_key"`
	RequestedModel string                `json:"requested_model"`
	VirtualModel   string                `json:"virtual_model,omitempty"`
	AttemptCount   int                   `json:"attempt_count"`
	Attempts       []notificationAttempt `json:"attempts,omitempty"`
	// Message, when set, is the full human-readable body for non-routing
	// (admin) events. It is rendered verbatim instead of the routing format.
	Message string `json:"message,omitempty"`
}

// notificationAttempt is the per-target outcome of a routed request.
type notificationAttempt struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Result       string `json:"result"`
	FailureClass string `json:"failure_class,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	LatencyMs    int64  `json:"latency_ms"`
}

// notificationSettings reads an account's notification configuration and
// applies the hosted-mode policy clamp. Every read of notification settings
// must go through this helper rather than calling the store directly, so the
// clamp cannot be bypassed at an individual call site (and so rows written
// before the clamp existed are corrected on read, not just on write).
func (s *Server) notificationSettings(ctx context.Context, sc *store.Scope) (store.NotificationSettings, error) {
	cfg, err := sc.GetNotificationSettingsBatch(ctx)
	if err != nil {
		return cfg, err
	}
	if s.config.Mode == config.ModeHosted {
		cfg.EventAdminLogin = false
		cfg.CooldownSeconds = hostedNotificationCooldownSeconds
	}
	return cfg, nil
}

// maybeNotify emits a single logical notification for a routed request based on
// its final outcome. The payload is built synchronously here (before the
// goroutine) because the caller's logRow continues to be mutated after this
// call. Only the delivery runs detached, so it never blocks or alters the
// client response.
func (s *Server) maybeNotify(row *logRow, route resolvedRoute, resp *http.Response) {
	var event string
	switch {
	case resp != nil && row.fallbackUsed:
		event = eventFallback
	case resp == nil && route.Virtual && hasFailedAttempt(row.attempts):
		event = eventAllFailed
	default:
		return
	}
	cfg, err := s.notificationSettings(context.Background(), s.scopeFor(row.accountID))
	if err != nil || !cfg.Enabled || cfg.WebhookURL == "" {
		return
	}
	if event == eventFallback && !cfg.EventFallback {
		return
	}
	if event == eventAllFailed && !cfg.EventAllFailed {
		return
	}
	payload := s.buildNotificationPayload(event, row, route)
	go s.deliverNotification(row.accountID, event, payload, cfg)
}

// subjectToCooldown reports whether an event is throttled by the notification
// cooldown. Only routing events (fallback, all-failed) are throttled; the manual
// test and discrete admin events are not.
func subjectToCooldown(event string) bool {
	return event == eventFallback || event == eventAllFailed
}

// notifyAdminEvent emits a best-effort notification for a discrete admin action
// (e.g. client key created/deleted, admin login). It is fire-and-forget and never
// blocks the admin request. The message is the full human-readable body.
func (s *Server) notifyAdminEvent(accountID, event, message string) {
	payload := notificationPayload{
		Event:     event,
		Severity:  severityInfo,
		Timestamp: database.Now(),
		Message:   message,
	}
	cfg, err := s.notificationSettings(context.Background(), s.scopeFor(accountID))
	if err != nil || !cfg.Enabled || cfg.WebhookURL == "" || !notificationEventEnabled(event, cfg) {
		return
	}
	go s.deliverNotification(accountID, event, payload, cfg)
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
// Notification delivery admission bounds. Per-event cooldown only throttles
// repeats of one key, so a burst of distinct events (client-key churn is exempt
// from cooldown entirely) could otherwise start unbounded concurrent deliveries
// against a slow endpoint. Saturation drops best-effort notifications rather
// than queueing them; delivery must never delay the request that triggered it.
const (
	maxConcurrentNotificationDeliveries          = 64
	maxConcurrentNotificationDeliveriesPerTenant = 8
)

// admitNotification reserves delivery capacity for one notification. It returns
// false when the process-wide or per-account bound is already reached, in which
// case the caller drops the notification. Callers must invoke
// releaseNotification exactly once for every true result.
func (s *Server) admitNotification(accountID string) bool {
	s.notifyCooldownMu.Lock()
	defer s.notifyCooldownMu.Unlock()
	if s.notifyAdmittedByAccount == nil {
		s.notifyAdmittedByAccount = map[string]int{}
	}
	if s.notifyAdmitted >= maxConcurrentNotificationDeliveries {
		return false
	}
	if s.notifyAdmittedByAccount[accountID] >= maxConcurrentNotificationDeliveriesPerTenant {
		return false
	}
	s.notifyAdmitted++
	s.notifyAdmittedByAccount[accountID]++
	return true
}

func (s *Server) releaseNotification(accountID string) {
	s.notifyCooldownMu.Lock()
	if s.notifyAdmitted > 0 {
		s.notifyAdmitted--
	}
	if n := s.notifyAdmittedByAccount[accountID]; n > 1 {
		s.notifyAdmittedByAccount[accountID] = n - 1
	} else {
		delete(s.notifyAdmittedByAccount, accountID)
	}
	s.notifyCooldownMu.Unlock()
}

// pruneNotificationCooldownsLocked drops cooldown entries that can no longer
// suppress anything (their window has elapsed for every configured cooldown).
// The map is keyed by account+event+model and would otherwise grow for the
// process lifetime as accounts and models churn. Caller holds notifyCooldownMu.
func (s *Server) pruneNotificationCooldownsLocked(now time.Time) {
	if len(s.notifyLastSent) == 0 {
		return
	}
	// The longest cooldown a tenant can configure is not known here, so use the
	// hosted floor as the retention window: any entry older than that can no
	// longer be within a sane cooldown.
	retention := time.Duration(hostedNotificationCooldownSeconds) * time.Second
	if retention < time.Minute {
		retention = time.Minute
	}
	for key, last := range s.notifyLastSent {
		if now.Sub(last) > retention {
			delete(s.notifyLastSent, key)
		}
	}
}

func (s *Server) deliverNotification(accountID, event string, payload notificationPayload, cfg store.NotificationSettings) {
	ctx := context.Background()
	if !cfg.Enabled || cfg.WebhookURL == "" {
		return
	}
	if !notificationEventEnabled(event, cfg) {
		return
	}
	// Bound aggregate outbound work before anything else. A rejection here is a
	// deliberate best-effort drop: the notification is lost and the triggering
	// request is unaffected.
	if !s.admitNotification(accountID) {
		s.logger.Warn("notification delivery dropped", "event", event, "reason", "delivery_capacity")
		return
	}
	defer s.releaseNotification(accountID)
	// Throttle repeat notifications for the same event + model within the
	// cooldown window. Reserve the key before starting delivery so concurrent
	// requests cannot fan out duplicate notifications. Only routing events are
	// throttled; the manual test and discrete admin events are not.
	key := accountID + "|" + event + "|" + payload.VirtualModel
	reserved := false
	if subjectToCooldown(event) {
		s.notifyCooldownMu.Lock()
		if s.notifyInFlight == nil {
			s.notifyInFlight = map[string]bool{}
		}
		if s.notifyLastSent == nil {
			s.notifyLastSent = map[string]time.Time{}
		}
		now := time.Now()
		s.pruneNotificationCooldownsLocked(now)
		if cfg.CooldownSeconds > 0 {
			if last, ok := s.notifyLastSent[key]; ok && now.Sub(last) < time.Duration(cfg.CooldownSeconds)*time.Second {
				s.notifyCooldownMu.Unlock()
				return
			}
		}
		if s.notifyInFlight[key] {
			s.notifyCooldownMu.Unlock()
			return
		}
		s.notifyInFlight[key] = true
		reserved = true
		s.notifyCooldownMu.Unlock()
	}
	if reserved {
		defer func() {
			s.notifyCooldownMu.Lock()
			delete(s.notifyInFlight, key)
			s.notifyCooldownMu.Unlock()
		}()
	}
	body := []byte(payload.humanMessage())
	reqCtx, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		s.logger.Warn("notification delivery failed", "event", event, "error_class", notificationErrorClass(err))
		return
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("X-Title", payload.heading())
	req.Header.Set("User-Agent", "Tiller-Router/1")
	if cfg.AuthHeader != "" {
		req.Header.Set("Authorization", cfg.AuthHeader)
	}
	resp, err := s.notifyClient.Do(req)
	if err != nil {
		s.logger.Warn("notification delivery failed", "event", event, "error_class", notificationErrorClass(err))
		return
	}
	if err := drainNotificationResponse(resp.Body); err != nil {
		_ = resp.Body.Close()
		s.logger.Warn("notification delivery failed", "event", event, "error_class", notificationErrorClass(err))
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.logger.Warn("notification delivery failed", "event", event, "status", resp.StatusCode)
		return
	}
	if reserved {
		s.notifyCooldownMu.Lock()
		s.notifyLastSent[key] = time.Now()
		s.notifyCooldownMu.Unlock()
	}
}

func notificationEventEnabled(event string, cfg store.NotificationSettings) bool {
	switch event {
	case eventFallback:
		return cfg.EventFallback
	case eventAllFailed:
		return cfg.EventAllFailed
	case eventClientKeyCreated:
		return cfg.EventClientKeyCreated
	case eventClientKeyDeleted:
		return cfg.EventClientKeyDeleted
	case eventAdminLogin:
		return cfg.EventAdminLogin
	default:
		return true
	}
}

func notificationErrorClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "transport"
}

// buildNotificationPayload assembles the metadata-only payload for an event
// from the request's routing outcome. Only metadata already recorded for
// Activity is used; no request or response content is ever included. It runs
// synchronously in the request goroutine (before delivery is spawned).
func (s *Server) buildNotificationPayload(event string, row *logRow, route resolvedRoute) notificationPayload {
	p := notificationPayload{
		Event:          event,
		Timestamp:      row.createdAt,
		ClientKey:      row.clientName,
		RequestedModel: row.requestedModel,
		VirtualModel:   route.RouteModel,
		AttemptCount:   attemptCount(row.attempts),
	}
	for _, a := range row.attempts {
		p.Attempts = append(p.Attempts, notificationAttempt{
			Provider:     a.provider,
			Model:        a.model,
			Result:       a.result,
			FailureClass: a.failureClass,
			HTTPStatus:   a.httpStatus,
			LatencyMs:    a.latencyMs,
		})
	}
	switch event {
	case eventFallback:
		p.Severity = severityWarning
	case eventAllFailed:
		p.Severity = severityError
	case eventTest:
		p.Severity = severityWarning
	}
	return p
}

// heading returns the short human-readable title for the notification. It is
// sent as the X-Title header so ntfy (and similar) render it as the heading.
// The model in question (the virtual model, falling back to the requested model)
// is included so the alert is identifiable at a glance.
func (p notificationPayload) heading() string {
	model := p.VirtualModel
	if model == "" {
		model = p.RequestedModel
	}
	switch p.Event {
	case eventFallback:
		return "Tiller Fallback - " + model
	case eventAllFailed:
		return "Tiller Routing Failed - " + model
	case eventTest:
		return "Tiller Test Notification"
	case eventClientKeyCreated:
		return "Tiller Client key created"
	case eventClientKeyDeleted:
		return "Tiller Client key deleted"
	case eventAdminLogin:
		return "Tiller Admin login"
	default:
		return "Tiller Notification"
	}
}

// humanMessage renders the payload as a human-readable plain-text message. It
// carries only metadata (the same privacy boundary as Activity) — no prompts,
// responses, tool arguments, reasoning, or credentials.
func (p notificationPayload) humanMessage() string {
	var b strings.Builder
	if p.Timestamp != "" {
		b.WriteString(formatTimestamp(p.Timestamp))
		b.WriteString("\n\n")
	}
	if p.Message != "" {
		b.WriteString(p.Message)
		return strings.TrimRight(b.String(), "\n")
	}
	if p.ClientKey != "" {
		fmt.Fprintf(&b, "Client: %s\n", p.ClientKey)
	}
	if p.RequestedModel != "" {
		fmt.Fprintf(&b, "Requested model: %s", p.RequestedModel)
		if p.VirtualModel != "" && p.VirtualModel != p.RequestedModel {
			fmt.Fprintf(&b, " Served model: %s", p.VirtualModel)
		}
		b.WriteString("\n")
	}
	failed := 0
	for _, a := range p.Attempts {
		switch a.Result {
		case "failed":
			failed++
			fmt.Fprintf(&b, "Failed #%d: %s/%s [%s, %dms]\n", failed, a.Provider, a.Model, failureMessage(a.FailureClass, a.HTTPStatus), a.LatencyMs)
		case "success":
			fmt.Fprintf(&b, "Succeeded: %s/%s [%dms]\n", a.Provider, a.Model, a.LatencyMs)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatTimestamp renders an RFC3339Nano timestamp as DD/MM/YYYY, HH:MM in UTC.
func formatTimestamp(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	return t.Format("02/01/2006, 15:04")
}

// failureMessage maps a machine-readable failure class (and optional HTTP status)
// to a short human-readable description for a failed attempt.
func failureMessage(class string, httpStatus int) string {
	if httpStatus > 0 {
		return fmt.Sprintf("HTTP %d", httpStatus)
	}
	switch class {
	case "unavailable":
		return "unavailable"
	case "protocol_unavailable":
		return "protocol not supported"
	case "free_model_requires_keyless":
		return "requires keyless provider"
	case "free_tier_rejected":
		return "free-tier policy rejection"
	case "invalid_upstream":
		return "invalid upstream"
	case "upstream_unreachable":
		return "unreachable"
	case "upstream_timeout":
		return "timeout"
	case "upstream_read_error":
		return "read error"
	case "provider_credentials_locked":
		return "credentials locked"
	case "cooldown":
		return "cooldown"
	case "client_timeout":
		return "client timeout"
	case "client_cancelled":
		return "client cancelled"
	default:
		return class
	}
}

// sendTestNotification sends a harmless "Tiller test notification" to the
// configured webhook and reports success/failure to the administrator. It uses
// the saved configuration (URL + optional auth header) regardless of the
// enabled flag so an admin can verify delivery before enabling events.
func (s *Server) sendTestNotification(w http.ResponseWriter, r *http.Request) {
	sc := s.scope(r)
	cfg, err := s.notificationSettings(r.Context(), sc)
	if err != nil {
		adminError(w, 500, "database_error", "Could not load notification settings.")
		return
	}
	if cfg.WebhookURL == "" {
		adminError(w, 400, "no_webhook_url", "Configure a webhook URL before sending a test notification.")
		return
	}
	if s.config.Mode == config.ModeHosted && hostednet.Validate(cfg.WebhookURL) != nil {
		adminError(w, 400, "invalid_webhook_url", "Hosted webhook URLs must use validated public HTTPS on port 443.")
		return
	}
	// Charge the budget before the outbound POST so a failing or slow webhook
	// cannot be used to burn through the allowance. The limiter is keyed by
	// account, so one tenant's spend never affects another's.
	if s.config.Mode == config.ModeHosted && !s.testNotificationLimiter.allowAttempt(sc.AccountID()) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Test notifications are limited to one per minute in hosted mode. Try again shortly.")
		return
	}
	payload := notificationPayload{
		Event:     eventTest,
		Severity:  severityWarning,
		Timestamp: database.Now(),
	}
	body := []byte(payload.humanMessage())
	ctx, cancel := context.WithTimeout(r.Context(), notificationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		adminError(w, 400, "invalid_webhook_url", "The configured webhook URL is invalid.")
		return
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("X-Title", payload.heading())
	req.Header.Set("User-Agent", "Tiller-Router/1")
	if cfg.AuthHeader != "" {
		req.Header.Set("Authorization", cfg.AuthHeader)
	}
	resp, err := s.notifyClient.Do(req)
	if err != nil {
		adminError(w, 502, "delivery_failed", "The test notification could not be delivered ("+notificationErrorClass(err)+").")
		return
	}
	if err := drainNotificationResponse(resp.Body); err != nil {
		_ = resp.Body.Close()
		adminError(w, 502, "delivery_failed", "The test notification response was too large or could not be read.")
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("The webhook returned HTTP %d.", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			msg += " The endpoint rejected the request — a saved Authorization header is being sent and may be invalid; clear it in the notifications settings if the endpoint doesn't require one."
		}
		adminError(w, 502, "delivery_failed", msg)
		return
	}
	writeJSON(w, 200, map[string]any{"delivered": true})
}

func drainNotificationResponse(body io.Reader) error {
	read, err := io.CopyN(io.Discard, body, maxNotificationResponseBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if read > maxNotificationResponseBytes {
		return errors.New("notification response body too large")
	}
	return nil
}
