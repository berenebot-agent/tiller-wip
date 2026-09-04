package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
)

// Live SSE refresh for the admin UI.
//
// A single admin-gated GET /api/admin/live stream pushes three event types:
//
//   - "outcome": a micro-delta of the in-memory per-target last request
//     outcomes, emitted the instant a routed request records one. This is what
//     makes the resolution icons feel live with zero DB cost.
//   - "activity": transient in-flight request deltas keyed by virtual model,
//     client key, or virtual-model/provider-model pair.
//   - "snapshot": the full usage/health envelope (last outcomes, 1h/24h health,
//     token + cache windows). Sent on connect and then on a server-side cadence
//     so token counters track traffic and any dropped delta self-heals.
//
// The dispatcher goroutine is the single owner of the aggregate recompute. It
// is lazily started on the first subscriber and stopped at zero subscribers, so
// no goroutine exists and no DB query runs while no admin tab is connected.
// Fan-out shares one marshalled []byte per event across all subscribers.

const (
	liveOutcomeBuffer    = 64
	liveDebounceInterval = 2 * time.Second
	liveIdleInterval     = 5 * time.Second
)

var liveSessionCheckInterval = time.Minute

// liveHub holds the subscriber set and the outcome delta channel. The
// dispatcher goroutine lifecycle is driven by subscribe/unsubscribe.
type liveHub struct {
	mu         sync.Mutex
	subs       map[chan []byte]struct{}
	outcomeCh  chan map[string]lastOutcome
	activityCh chan inflightDelta
	cancel     context.CancelFunc
	// snapshot recomputes the full usage/health envelope. It is bound to the
	// owning Server so the dispatcher and the /api/admin/usage endpoint share
	// one source of truth.
	snapshot func(context.Context) (liveSnapshot, error)
}

func (h *liveHub) emitActivity(delta inflightDelta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return
	}
	select {
	case h.activityCh <- delta:
	default:
	}
}

// emitOutcome publishes only while a live subscriber exists. The subscriber
// check and channel send share the hub lock so an outcome cannot be queued
// after the last subscriber leaves.
func (h *liveHub) emitOutcome(delta map[string]lastOutcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return
	}
	select {
	case h.outcomeCh <- delta:
	default:
	}
}

// liveSnapshot is the full envelope pushed on the "snapshot" event. It is the
// same data the /api/admin/usage endpoint returns, so the two can never drift.
type liveSnapshot struct {
	GeneratedAt       string                            `json:"generated_at"`
	TargetLastOutcome map[string]lastOutcome            `json:"target_last_outcome"`
	TargetHealth      map[string]targetResolutionHealth `json:"target_health"`
	VirtualModels     map[string]usageWindows           `json:"virtual_models"`
	ClientKeys        map[string]usageWindows           `json:"client_keys"`
	RealModels        map[string]usageWindows           `json:"real_models"`
	VirtualCache      map[string]cacheWindows           `json:"virtual_cache"`
	ClientCache       map[string]cacheWindows           `json:"client_cache"`
	RealCache         map[string]cacheWindows           `json:"real_cache"`
	// Modules carries current aggregate state for live UI modules, including
	// in-flight virtual-model requests.
	Modules map[string]any `json:"modules"`
}

// subscribe registers a new subscriber and lazily starts the dispatcher if this
// is the first one. The returned channel receives pre-marshalled SSE messages.
func (h *liveHub) subscribe() chan []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = make(map[chan []byte]struct{})
	}
	ch := make(chan []byte, 8)
	h.subs[ch] = struct{}{}
	if h.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.dispatcher(ctx)
	}
	return ch
}

// unsubscribe removes a subscriber and stops the dispatcher when the last one
// leaves. A brief overlap with a freshly-started dispatcher is harmless: both
// only broadcast snapshots, and the old one exits on its cancelled context.
func (h *liveHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ch)
	if len(h.subs) == 0 && h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
}

// broadcast formats one SSE message and fans it out to every subscriber. A
// full or slow subscriber drops the message; the next snapshot reconciles it.
func (h *liveHub) broadcast(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	msg := []byte("event: " + event + "\ndata: " + string(data) + "\n\n")
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// dispatcher is the single owner of the aggregate recompute. It broadcasts an
// outcome delta immediately, then coalesces the full snapshot behind a 2s
// debounce under load and a 5s idle ticker otherwise.
func (h *liveHub) dispatcher(ctx context.Context) {
	debounce := time.NewTimer(liveDebounceInterval)
	if !debounce.Stop() {
		select {
		case <-debounce.C:
		default:
		}
	}
	idle := time.NewTicker(liveIdleInterval)
	defer idle.Stop()
	defer debounce.Stop()
	dirty := false
	for {
		select {
		case <-ctx.Done():
			return
		case outcomes := <-h.outcomeCh:
			h.broadcast("outcome", outcomes)
			dirty = true
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(liveDebounceInterval)
		case delta := <-h.activityCh:
			h.broadcast("activity", delta)
		case <-debounce.C:
			if dirty {
				h.broadcastSnapshot()
				dirty = false
			}
		case <-idle.C:
			h.broadcastSnapshot()
			dirty = false
		}
	}
}

// broadcastSnapshot recomputes and pushes the full envelope. It is the
// self-healing source of truth; a dropped outcome delta is corrected here.
func (h *liveHub) broadcastSnapshot() {
	if h.snapshot == nil {
		return
	}
	snap, err := h.snapshot(context.Background())
	if err != nil {
		return
	}
	h.broadcast("snapshot", snap)
}

// live is the SSE handler. It is admin-gated (GET, cookie auth, CSRF-exempt).
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		adminError(w, http.StatusInternalServerError, "streaming_unsupported", "Streaming is not supported.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.liveHub.subscribe()
	defer s.liveHub.unsubscribe(ch)
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return
	}
	session := r.Context().Value(adminSessionKey).(auth.Session)
	validate := time.NewTicker(liveSessionCheckInterval)
	defer validate.Stop()
	expires := time.NewTimer(time.Until(session.ExpiresAt))
	defer expires.Stop()

	// Baseline snapshot on connect (and reconnect) so the client reconciles
	// anything it may have missed while disconnected.
	if snap, err := s.buildUsageSnapshot(r.Context()); err == nil {
		if data, err := json.Marshal(snap); err == nil {
			_, _ = w.Write([]byte("event: snapshot\ndata: " + string(data) + "\n\n"))
			flusher.Flush()
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-expires.C:
			return
		case <-validate.C:
			current, ok := s.sessions.Validate(cookie.Value)
			if !ok {
				return
			}
			if !expires.Stop() {
				select {
				case <-expires.C:
				default:
				}
			}
			expires.Reset(time.Until(current.ExpiresAt))
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
