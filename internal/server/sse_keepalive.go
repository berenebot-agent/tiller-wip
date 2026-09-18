package server

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// sseKeepaliveInterval is how long a streaming response may sit without a write
// before a comment frame is emitted. Reverse proxies commonly apply an
// idle-read timeout to the upstream (Tiller) leg; a long reasoning prefill can
// exceed it with no SSE events to forward, so a periodic comment keeps the
// connection alive without altering the event stream.
const sseKeepaliveInterval = 15 * time.Second

// sseKeepaliveWriter wraps a streaming response writer and emits
// ": keepalive\n\n" comment frames during silence. Every write resets the
// timer. A mutex serializes the timer goroutine and the streaming goroutine so
// concurrent writes to the underlying ResponseWriter are safe.
type sseKeepaliveWriter struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	mu       sync.Mutex
	timer    *time.Timer
	interval time.Duration
	stopped  bool
}

func newSSEKeepaliveWriter(w http.ResponseWriter, interval time.Duration) *sseKeepaliveWriter {
	k := &sseKeepaliveWriter{w: w, interval: interval}
	if f, ok := w.(http.Flusher); ok {
		k.flusher = f
	}
	if interval > 0 {
		k.timer = time.AfterFunc(interval, k.keepalive)
	}
	return k
}

func (k *sseKeepaliveWriter) keepalive() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped || k.timer == nil {
		return
	}
	_, _ = io.WriteString(k.w, ": keepalive\n\n")
	if k.flusher != nil {
		k.flusher.Flush()
	}
	k.timer.Reset(k.interval)
}

func (k *sseKeepaliveWriter) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return 0, io.ErrClosedPipe
	}
	n, err := k.w.Write(p)
	if k.timer != nil {
		k.timer.Reset(k.interval)
	}
	return n, err
}

func (k *sseKeepaliveWriter) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.flusher != nil {
		k.flusher.Flush()
	}
}

// Close stops the keepalive timer and blocks until any in-flight keepalive
// write completes, so the underlying ResponseWriter is never used after the
// handler returns.
func (k *sseKeepaliveWriter) Close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopped = true
	if k.timer != nil {
		k.timer.Stop()
	}
}
