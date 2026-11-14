package server

import (
	"errors"
	"net/http"
	"os"
	"sync"
	"time"
)

// Inbound request-body admission.
//
// Byte limits (MaxBytesReader) bound how much a request may send but not how
// long it may take. Without a read deadline, a peer can trickle a body
// indefinitely: the handler goroutine, its connection, and (for inference) the
// up-to-8 MiB buffer are all held with no admission check, because plan
// concurrency is only applied after the body is parsed. The deadline below
// bounds one request; the gate bounds how many may be mid-body at once.
const (
	// maxConcurrentBodyReads caps how many requests may be reading a body at
	// once process-wide. The product of the gate and the largest body cap is
	// the process's worst-case body-buffer budget: 64 x 8 MiB = 512 MiB. The
	// pre-release review (../docs/pre_saas_release_review.md TR-002) sized both
	// constants so the aggregate fits well inside a small container; a single
	// flood cannot hold gigabytes in unread request bodies.
	maxConcurrentBodyReads = 64
	// maxConcurrentBodyReadsPerAccount caps one account's share of the
	// inference gate so a single tenant trickling bodies cannot hold every slot
	// and deny inference to every other tenant. It applies in both modes (local
	// mode has one account, so its effective ceiling is this value).
	maxConcurrentBodyReadsPerAccount = 16
	// maxConcurrentPreAuthBodyReads sizes the separate gate for request bodies
	// read before authentication (passkey sign-in finish). It is disjoint from
	// the inference gate, so an anonymous client can never consume the
	// capacity inference depends on.
	maxConcurrentPreAuthBodyReads = 8
	// maxConcurrentAuthBodyReads bounds UNAUTHENTICATED auth-surface body reads
	// (login, signup, password reset, platform login) in their own pool. It is
	// sized above a realistic login burst because each of those bodies is
	// capped at authRequestMaxBytes (8 KiB); its job is to keep anonymous
	// bodies off the inference gate while still bounding how many may be
	// mid-read at once. Authenticated auth-surface requests charge their
	// account's share of the inference gate instead.
	maxConcurrentAuthBodyReads = 32
)

// bodyReadDeadline is an absolute deadline for reading one request body,
// measured from when the read starts; it is not reset by progress. Five minutes
// matches the upstream idle timeout and lets a legitimate 8 MiB inference body
// arrive over a slow link. It is a variable so tests can shorten it.
var bodyReadDeadline = 5 * time.Minute

// preAuthBodyReadDeadline is the absolute body deadline for small
// pre-authentication bodies (a WebAuthn assertion is a few KiB), so a stalled
// anonymous upload releases its pre-auth slot quickly.
var preAuthBodyReadDeadline = 15 * time.Second

// errBodyReadTimeout is returned when a client did not send a complete body
// within the read deadline, so callers can distinguish a stalled upload from
// malformed content.
var errBodyReadTimeout = errors.New("request body was not received in time")

// errBodyReadBusy reports that the admission gate refused a body read because
// too many are already in progress. It is a capacity answer, not a bad request.
var errBodyReadBusy = errors.New("too many request bodies in progress")

// bodyReadGate bounds concurrent request-body reads, in aggregate and
// optionally per key (account). A nil gate (constructed only in tests that
// bypass the server) admits everything. A zero max means
// maxConcurrentBodyReads; a zero perKey means no per-key cap.
type bodyReadGate struct {
	max    int
	perKey int

	mu     sync.Mutex
	active int
	byKey  map[string]int
}

func (g *bodyReadGate) acquire(key string) bool {
	if g == nil {
		return true
	}
	limit := g.max
	if limit <= 0 {
		limit = maxConcurrentBodyReads
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active >= limit {
		return false
	}
	if g.perKey > 0 && g.byKey[key] >= g.perKey {
		return false
	}
	g.active++
	if g.perKey > 0 {
		if g.byKey == nil {
			g.byKey = map[string]int{}
		}
		g.byKey[key]++
	}
	return true
}

func (g *bodyReadGate) release(key string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.perKey > 0 {
		if n := g.byKey[key] - 1; n > 0 {
			g.byKey[key] = n
		} else {
			delete(g.byKey, key)
		}
	}
}

// inUse reports the aggregate number of held slots.
func (g *bodyReadGate) inUse() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

// withBodyReadDeadline runs fn with a read deadline applied to the underlying
// connection, clearing it afterwards so the response write is not affected.
//
// The deadline is applied through http.ResponseController so it works on a real
// server and degrades to a no-op on an http.ResponseWriter that does not expose
// the connection (httptest.NewRecorder, some middleware wrappers). A failure to
// set it is not fatal: the byte caps and the gate still apply.
func withBodyReadDeadline(w http.ResponseWriter, fn func() error) error {
	return withBodyReadDeadlineFor(w, bodyReadDeadline, fn)
}

// withBodyReadDeadlineFor is withBodyReadDeadline with an explicit deadline.
func withBodyReadDeadlineFor(w http.ResponseWriter, d time.Duration, fn func() error) error {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(d)); err != nil {
		// Unsupported writer: run without the deadline rather than fail the
		// request. This is the httptest and wrapped-writer path.
		return fn()
	}
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	return fn()
}

// isTimeoutError reports whether an error came from a read deadline rather than
// a client disconnect or a malformed body.
func isTimeoutError(err error) bool {
	if errors.Is(err, errBodyReadTimeout) {
		return true
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// respondDecodeError writes the correct response for a JSON body decode
// failure: 408 for a stalled upload (the read deadline expired) and 400 for a
// malformed or oversized body. Every decode call site uses this so a slow
// client gets an accurate status instead of a misleading "invalid JSON".
func respondDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errBodyReadTimeout) {
		adminError(w, http.StatusRequestTimeout, "request_timeout", "The request body was not received in time.")
		return
	}
	if errors.Is(err, errBodyReadBusy) {
		w.Header().Set("Retry-After", "1")
		adminError(w, http.StatusServiceUnavailable, "body_read_busy", "Too many requests are being received at once. Try again.")
		return
	}
	adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
}
