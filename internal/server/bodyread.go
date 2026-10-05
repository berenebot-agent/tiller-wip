package server

import (
	"errors"
	"net/http"
	"os"
	"sync/atomic"
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
	// pre-release review (docs/pre_saas_release_review.md TR-002) sized both
	// constants so the aggregate fits well inside a small container; a single
	// flood cannot hold gigabytes in unread request bodies.
	maxConcurrentBodyReads = 64
)

// bodyReadDeadline is an inactivity deadline per read, not a total upload
// budget: a slow but progressing upload on a poor link is never cut off,
// because every successful read resets it. Five minutes matches the upstream
// idle timeout and is far longer than any legitimate gap between chunks of a
// request body. It is a variable so tests can shorten it.
var bodyReadDeadline = 5 * time.Minute

// errBodyReadTimeout is returned when a client did not send a complete body
// within the read deadline, so callers can distinguish a stalled upload from
// malformed content.
var errBodyReadTimeout = errors.New("request body was not received in time")

// bodyReadGate bounds concurrent request-body reads. A nil gate (constructed
// only in tests that bypass the server) admits everything.
type bodyReadGate struct {
	active atomic.Int64
}

func (g *bodyReadGate) acquire() bool {
	if g == nil {
		return true
	}
	for {
		cur := g.active.Load()
		if cur >= maxConcurrentBodyReads {
			return false
		}
		if g.active.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (g *bodyReadGate) release() {
	if g == nil {
		return
	}
	g.active.Add(-1)
}

// withBodyReadDeadline runs fn with a read deadline applied to the underlying
// connection, clearing it afterwards so the response write is not affected.
//
// The deadline is applied through http.ResponseController so it works on a real
// server and degrades to a no-op on an http.ResponseWriter that does not expose
// the connection (httptest.NewRecorder, some middleware wrappers). A failure to
// set it is not fatal: the byte caps and the gate still apply.
func withBodyReadDeadline(w http.ResponseWriter, fn func() error) error {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(bodyReadDeadline)); err != nil {
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
	adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
}
